// Package db implements SQL transactions with durable redo logging.
package db

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Column struct {
	Name string
	Type string
	Dim  int
}
type Table struct {
	Columns []Column
	Rows    [][]any
}
type State map[string]*Table
type Result struct {
	Columns  []string `json:"columns,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"`
	Affected int      `json:"affected"`
	Error    string   `json:"error,omitempty"`
}
type Engine struct {
	mu      sync.Mutex
	dir     string
	state   State
	version uint64
	walErr  error
}
type Session struct {
	e          *Engine
	statements []string
	tx         State
	version    uint64
}

func (e *Engine) Session() *Session { return &Session{e: e} }
func clone(s State) State           { b, _ := json.Marshal(s); var t State; _ = json.Unmarshal(b, &t); return t }

func (s *Session) Execute(sql string) Result {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	r, err := s.execute(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";")))
	if err != nil {
		return Result{Error: err.Error()}
	}
	return r
}
func (s *Session) execute(sql string) (Result, error) {
	switch strings.ToUpper(sql) {
	case "BEGIN", "START TRANSACTION":
		if s.tx != nil {
			return Result{}, errors.New("transaction already active")
		}
		s.statements = nil
		s.tx = clone(s.e.state)
		s.version = s.e.version
		return Result{}, nil
	case "ROLLBACK":
		s.statements = nil
		s.tx = nil
		return Result{}, nil
	case "COMMIT":
		if s.tx == nil {
			return Result{}, nil
		}
		if s.version != s.e.version {
			return Result{}, errors.New("transaction conflict; ROLLBACK required")
		}
		if err := s.e.persist(s.statements); err != nil {
			return Result{}, err
		}
		if len(s.statements) > 0 {
			s.e.version++
		}
		s.e.state = s.tx
		s.tx = nil
		s.statements = nil
		return Result{}, nil
	}
	state := s.e.state
	if s.tx != nil {
		state = s.tx
	}
	next := clone(state)
	r, write, err := run(next, sql)
	if err != nil {
		return r, err
	}
	if !write {
		return r, nil
	}
	if s.tx != nil {
		s.statements = append(s.statements, sql)
		s.tx = next
		return r, nil
	}
	if err = s.e.persist([]string{sql}); err != nil {
		return Result{}, err
	}
	s.e.state = next
	s.e.version++
	return r, nil
}

var create = regexp.MustCompile(`(?is)^CREATE TABLE (\w+)\s*\((.*)\)$`)
var insert = regexp.MustCompile(`(?is)^INSERT INTO (\w+) VALUES\s*\((.*)\)$`)
var selectRE = regexp.MustCompile(`(?is)^SELECT \* FROM (\w+)(?: ORDER BY (L2_DISTANCE|DISTANCE|COSINE|DOT|EUCLIDEAN)\((\w+),\s*(\[.*\])\)(?: (ASC|DESC))?)?(?: LIMIT (\d+))?$`)
var remove = regexp.MustCompile(`(?is)^(DROP TABLE|DELETE FROM) (\w+)$`)
var columnRE = regexp.MustCompile(`(?i)^(\w+)\s+(BIGINT|INT|INTEGER|VARCHAR|TEXT|FLOAT|DOUBLE|VECTOR)(?:\((\d+)\))?$`)

// split respects quoted SQL strings, nested parentheses, and vector brackets.
func split(v string) ([]string, error) {
	var out []string
	start, depth := 0, 0
	quoted := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\'' {
			if quoted && i+1 < len(v) && v[i+1] == '\'' {
				i++
				continue
			}
			quoted = !quoted
		}
		if quoted {
			continue
		}
		switch c {
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(v[start:i]))
				start = i + 1
			}
		}
		if depth < 0 {
			return nil, errors.New("unbalanced value")
		}
	}
	if quoted || depth != 0 {
		return nil, errors.New("unbalanced value")
	}
	return append(out, strings.TrimSpace(v[start:])), nil
}
func vector(v any) ([]float64, error) {
	b, _ := json.Marshal(v)
	var a []float64
	err := json.Unmarshal(b, &a)
	if err == nil && len(a) == 0 {
		err = errors.New("empty vector")
	}
	return a, err
}
func run(state State, sql string) (Result, bool, error) {
	if strings.EqualFold(sql, "SELECT 1") {
		return Result{Columns: []string{"1"}, Rows: [][]any{{float64(1)}}}, false, nil
	}
	fail := func(msg string) (Result, bool, error) { return Result{}, false, errors.New(msg) }
	if m := create.FindStringSubmatch(sql); m != nil {
		name := strings.ToLower(m[1])
		if state[name] != nil {
			return fail("table exists")
		}
		parts, err := split(m[2])
		if err != nil {
			return fail(err.Error())
		}
		t := &Table{}
		seen := map[string]bool{}
		for _, p := range parts {
			c := columnRE.FindStringSubmatch(p)
			if c == nil {
				return fail("unsupported column definition")
			}
			col := Column{Name: strings.ToLower(c[1]), Type: strings.ToUpper(c[2])}
			if seen[col.Name] {
				return fail("duplicate column")
			}
			seen[col.Name] = true
			if col.Type == "VECTOR" {
				col.Dim, _ = strconv.Atoi(c[3])
				if col.Dim < 1 {
					return fail("VECTOR requires positive dimension")
				}
			} else if c[3] != "" && col.Type != "VARCHAR" {
				return fail("unexpected dimension")
			}
			t.Columns = append(t.Columns, col)
		}
		state[name] = t
		return Result{}, true, nil
	}
	if m := insert.FindStringSubmatch(sql); m != nil {
		t := state[strings.ToLower(m[1])]
		if t == nil {
			return fail("unknown table")
		}
		parts, err := split(m[2])
		if err != nil {
			return fail(err.Error())
		}
		if len(parts) != len(t.Columns) {
			return fail("column count mismatch")
		}
		row := make([]any, len(parts))
		for i, p := range parts {
			c := t.Columns[i]
			switch c.Type {
			case "TEXT", "VARCHAR":
				if len(p) < 2 || p[0] != '\'' || p[len(p)-1] != '\'' {
					return fail("TEXT requires SQL quotes")
				}
				row[i] = strings.ReplaceAll(p[1:len(p)-1], "''", "'")
			case "VECTOR":
				var v []float64
				if json.Unmarshal([]byte(p), &v) != nil || len(v) != c.Dim {
					return fail("vector dimension or syntax mismatch")
				}
				row[i] = v
			default:
				n, err := strconv.ParseFloat(p, 64)
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
					return fail("invalid number")
				}
				if (c.Type == "INT" || c.Type == "INTEGER" || c.Type == "BIGINT") && n != math.Trunc(n) {
					return fail("integer required")
				}
				row[i] = n
			}
		}
		t.Rows = append(t.Rows, row)
		return Result{Affected: 1}, true, nil
	}
	if m := remove.FindStringSubmatch(sql); m != nil {
		n := strings.ToLower(m[2])
		t := state[n]
		if t == nil {
			return fail("unknown table")
		}
		count := len(t.Rows)
		if strings.EqualFold(m[1], "DROP TABLE") {
			delete(state, n)
		} else {
			t.Rows = nil
		}
		return Result{Affected: count}, true, nil
	}
	if m := selectRE.FindStringSubmatch(sql); m != nil {
		t := state[strings.ToLower(m[1])]
		if t == nil {
			return fail("unknown table")
		}
		r := Result{Rows: t.Rows}
		for _, c := range t.Columns {
			r.Columns = append(r.Columns, c.Name)
		}
		if m[2] != "" {
			idx := -1
			for i, c := range t.Columns {
				if c.Name == strings.ToLower(m[3]) && c.Type == "VECTOR" {
					idx = i
				}
			}
			if idx < 0 {
				return fail("unknown vector column")
			}
			var q []float64
			if json.Unmarshal([]byte(m[4]), &q) != nil || len(q) != t.Columns[idx].Dim {
				return fail("query vector dimension mismatch")
			}
			scores := make(map[int]float64)
			_ = scores
			type scored struct {
				row   []any
				score float64
			}
			items := make([]scored, len(r.Rows))
			for i, row := range r.Rows {
				v, err := vector(row[idx])
				if err != nil {
					return fail(err.Error())
				}
				score := distance(strings.ToUpper(m[2]), v, q)
				items[i] = scored{row, score}
			}
			sort.SliceStable(items, func(i, j int) bool {
				if strings.EqualFold(m[5], "DESC") {
					return items[i].score > items[j].score
				}
				return items[i].score < items[j].score
			})
			r.Rows = make([][]any, len(items))
			for i, item := range items {
				r.Rows[i] = item.row
			}
		}
		if m[6] != "" {
			n, err := strconv.Atoi(m[6])
			if err != nil {
				return fail("invalid LIMIT")
			}
			if n < len(r.Rows) {
				r.Rows = r.Rows[:n]
			}
		}
		return r, false, nil
	}
	return fail("unsupported SQL; see README.md for supported grammar")
}

// COSINE is cosine distance; DOT is negative inner product (smaller is nearer).
func distance(op string, a, b []float64) float64 {
	var dot, aa, bb, sq float64
	for i, x := range a {
		y := b[i]
		dot += x * y
		aa += x * x
		bb += y * y
		sq += (x - y) * (x - y)
	}
	switch op {
	case "DOT":
		return -dot
	case "COSINE":
		if aa == 0 || bb == 0 {
			return 1
		}
		return 1 - dot/math.Sqrt(aa*bb)
	default:
		return math.Sqrt(sq)
	}
}
