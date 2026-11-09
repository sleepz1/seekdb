// Package db implements SQL transactions with durable redo logging.
package db

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Column struct {
	Name    string
	Type    string
	Dim     int
	CharLen int
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
	mu         sync.Mutex
	dir        string
	state      State
	version    uint64
	walErr     error
	defaultANN bool
}
type Session struct {
	e          *Engine
	statements []string
	tx         State
	version    uint64
	ann        bool
}

// SetDefaultANN configures new sessions; existing sessions retain their mode.
func (e *Engine) SetDefaultANN(ann bool) { e.mu.Lock(); defer e.mu.Unlock(); e.defaultANN = ann }
func (e *Engine) Session() *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &Session{e: e, ann: e.defaultANN}
}
func clone(s State) State {
	t := State{}
	for name, table := range s {
		n := &Table{Columns: append([]Column(nil), table.Columns...)}
		for _, row := range table.Rows {
			r := append([]any(nil), row...)
			for i, v := range r {
				if a, ok := v.([]float32); ok {
					r[i] = append([]float32(nil), a...)
				}
			}
			n.Rows = append(n.Rows, r)
		}
		t[name] = n
	}
	return t
}

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
	if m := modeRE.FindStringSubmatch(sql); m != nil {
		s.ann = strings.EqualFold(m[1], "ann")
		return Result{}, nil
	}
	if strings.EqualFold(sql, "SELECT search_mode") {
		mode := "exact"
		if s.ann {
			mode = "ann"
		}
		return Result{Columns: []string{"search_mode"}, Rows: [][]any{{mode}}}, nil
	}
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
	r, write, err := runMode(next, sql, s.ann)
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

var modeRE = regexp.MustCompile(`(?i)^SET search_mode\s*=\s*(ann|exact)$`)
var create = regexp.MustCompile(`(?is)^CREATE TABLE (\w+)\s*\((.*)\)$`)
var insert = regexp.MustCompile(`(?is)^INSERT INTO (\w+) VALUES\s*\((.*)\)$`)
var selectRE = regexp.MustCompile(`(?is)^SELECT \* FROM (\w+)(?: WHERE (.*?))?(?: ORDER BY (L2_DISTANCE|DISTANCE|COSINE|DOT|EUCLIDEAN)\((\w+),\s*(\[.*\])\)(?: (ASC|DESC))?)?(?: LIMIT (\d+))?$`)
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
func run(state State, sql string) (Result, bool, error) { return runMode(state, sql, false) }
func runMode(state State, sql string, ann bool) (Result, bool, error) {
	if strings.EqualFold(sql, "SELECT 1") {
		return Result{Columns: []string{"1"}, Rows: [][]any{{int64(1)}}}, false, nil
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
				var err error
				col.Dim, err = strconv.Atoi(c[3])
				if err != nil || col.Dim < 1 {
					return fail("VECTOR requires positive dimension")
				}
			} else if c[3] != "" && col.Type != "VARCHAR" {
				return fail("unexpected dimension")
			}
			if col.Type == "VARCHAR" && c[3] != "" {
				var err error
				col.CharLen, err = strconv.Atoi(c[3])
				if err != nil || col.CharLen < 1 {
					return fail("VARCHAR requires positive length")
				}
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
			v, err := parseValue(t.Columns[i], p)
			if err != nil {
				return fail(err.Error())
			}
			row[i] = v
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
		r := Result{}
		for _, c := range t.Columns {
			r.Columns = append(r.Columns, c.Name)
		}
		predicates, err := parseWhere(t, m[2])
		if err != nil {
			return fail(err.Error())
		}
		for _, row := range t.Rows {
			match := true
			for _, p := range predicates {
				if row[p.index] != p.value {
					match = false
					break
				}
			}
			if match {
				r.Rows = append(r.Rows, row)
			}
		}
		if m[3] != "" {
			idx, pk := -1, -1
			for i, c := range t.Columns {
				if c.Name == strings.ToLower(m[4]) && c.Type == "VECTOR" {
					idx = i
				}
				if pk < 0 && c.Type == "BIGINT" {
					pk = i
				}
			}
			if idx < 0 {
				return fail("unknown vector column")
			}
			q, err := parseVector(m[5], t.Columns[idx].Dim)
			if err != nil {
				return fail("query " + err.Error())
			}
			if ann {
				r.Rows = annCandidates(r.Rows)
			}
			type scored struct {
				row   []any
				score float32
			}
			items := make([]scored, len(r.Rows))
			for i, row := range r.Rows {
				score := distance(strings.ToUpper(m[3]), row[idx].([]float32), q)
				if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
					return fail("float32 distance computation overflow")
				}
				items[i] = scored{row, score}
			}
			sort.SliceStable(items, func(i, j int) bool {
				if items[i].score == items[j].score {
					if pk >= 0 {
						return items[i].row[pk].(int64) < items[j].row[pk].(int64)
					}
					return false
				}
				if strings.EqualFold(m[6], "DESC") {
					return items[i].score > items[j].score
				}
				return items[i].score < items[j].score
			})
			r.Rows = make([][]any, len(items))
			for i, item := range items {
				r.Rows[i] = item.row
			}
		}
		if m[7] != "" {
			n, err := strconv.Atoi(m[7])
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
func distance(op string, a, b []float32) float32 {
	var dot, aa, bb, sq float32
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
		if math.IsInf(float64(aa), 0) || math.IsInf(float64(bb), 0) {
			return float32(math.NaN())
		}
		if aa == 0 || bb == 0 {
			return 1
		}
		return 1 - dot/float32(math.Sqrt(float64(aa))*math.Sqrt(float64(bb)))
	default:
		return float32(math.Sqrt(float64(sq)))
	}
}
