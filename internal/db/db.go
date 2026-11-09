// Package db implements SQL transactions with durable redo logging.
package db

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type Column struct {
	Primary bool
	Name    string
	Type    string
	Dim     int
	CharLen int
}
type Table struct {
	pages   *pageStore
	refs    []pageRef
	Indexes []VectorIndex
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
	checkpointVersion uint64
	walBytes          int64
	activeTx          int
	ready             bool
	pages             *pageStore
	mu                sync.Mutex
	dir               string
	state             State
	snapshotLSN       uint64
	version           uint64
	walErr            error
	defaultANN        bool
}
type Session struct {
	spool      string
	txWrites   int
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
		n := &Table{Columns: append([]Column(nil), table.Columns...), pages: table.pages, refs: append([]pageRef(nil), table.refs...)}
		for _, idx := range table.Indexes {
			n.Indexes = append(n.Indexes, VectorIndex{Name: idx.Name, Column: idx.Column, buckets: idx.buckets})
		}
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
	if !s.e.ready {
		return Result{Error: "database not ready"}
	}
	defer s.e.maybeCheckpoint()
	r, err := s.execute(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";")))
	if err != nil {
		return Result{Error: err.Error()}
	}
	return r
}
func (s *Session) execute(sql string) (Result, error) {
	sql = normalizeSQL(sql)
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
		s.clearStatements()
		s.tx = clone(s.e.state)
		s.e.activeTx++
		s.version = s.e.version
		return Result{}, nil
	case "ROLLBACK":
		if s.tx != nil {
			s.e.activeTx--
		}
		s.clearStatements()
		s.tx = nil
		return Result{}, nil
	case "COMMIT":
		if s.tx == nil {
			return Result{}, nil
		}
		if s.version != s.e.version {
			return Result{}, errors.New("transaction conflict; ROLLBACK required")
		}
		if err := s.persistStatements(); err != nil {
			return Result{}, err
		}
		if s.txWrites > 0 {
			s.e.version++
		}
		s.e.state = s.tx
		s.e.activeTx--
		s.tx = nil
		s.clearStatements()
		s.e.maybeCompact()
		return Result{}, nil
	}
	state := s.e.state
	if s.tx != nil {
		state = s.tx
	}
	if strings.HasPrefix(strings.ToUpper(sql), "SELECT ") {
		r, _, err := runMode(state, sql, s.ann)
		return r, err
	}
	next := clone(state)
	r, write, err := runStored(next, sql, s.ann, s.e.pages)
	if err != nil {
		return r, err
	}
	if !write {
		return r, nil
	}
	if s.tx != nil {
		if err := s.recordStatement(sql); err != nil {
			return Result{}, err
		}
		s.tx = next
		return r, nil
	}
	if err = s.e.persist([]string{sql}); err != nil {
		return Result{}, err
	}
	s.e.state = next
	s.e.version++
	s.e.maybeCompact()
	return r, nil
}

var modeRE = regexp.MustCompile(`(?i)^SET search_mode\s*=\s*(ann|exact)$`)
var create = regexp.MustCompile(`(?is)^CREATE TABLE (\w+)\s*\((.*)\)$`)
var insert = regexp.MustCompile(`(?is)^INSERT INTO (\w+) VALUES\s*(.*)$`)
var remove = regexp.MustCompile(`(?is)^(DROP TABLE|DELETE FROM) (\w+)$`)
var columnRE = regexp.MustCompile(`(?i)^(\w+)\s+(BIGINT|INT|INTEGER|VARCHAR|TEXT|FLOAT|DOUBLE|VECTOR)(?:\((\d+)\))?(\s+PRIMARY KEY)?$`)

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
func runMode(state State, sql string, ann bool) (result Result, write bool, err error) {
	return runStored(state, sql, ann, nil)
}
func runStored(state State, sql string, ann bool, pages *pageStore) (Result, bool, error) {
	return runStoredMaintenance(state, sql, ann, pages, true)
}

// Recovery defers derived index maintenance until all authoritative writes have
// replayed. Replay uses exact mode; mutation validation does not consult buckets.
func runStoredMaintenance(state State, sql string, ann bool, pages *pageStore, rebuild bool) (result Result, write bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(storageError); ok {
				result = Result{}
				write = false
				err = e.error
			} else {
				panic(v)
			}
		}
	}()
	sql = normalizeSQL(sql)
	defer func() {
		if rebuild && write && err == nil {
			for _, t := range state {
				rebuildIndexes(t)
			}
		}
	}()
	if m := indexRE.FindStringSubmatch(sql); m != nil {
		err := createIndex(state, m[1], m[2], m[3])
		return Result{}, err == nil, err
	}
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
		t := &Table{pages: pages}
		seen := map[string]bool{}
		pkCount, vectors := 0, 0
		for _, p := range parts {
			c := columnRE.FindStringSubmatch(p)
			if c == nil {
				return fail("unsupported column definition")
			}
			col := Column{Name: strings.ToLower(c[1]), Type: strings.ToUpper(c[2])}
			col.Primary = c[4] != ""
			if col.Primary {
				if col.Type != "BIGINT" {
					return fail("PRIMARY KEY requires BIGINT")
				}
				pkCount++
			}
			if seen[col.Name] {
				return fail("duplicate column")
			}
			seen[col.Name] = true
			if col.Type == "VECTOR" {
				vectors++
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
		if pkCount != 1 || vectors > 1 {
			return fail("requires one BIGINT PRIMARY KEY and at most one VECTOR")
		}
		state[name] = t
		return Result{}, true, nil
	}
	if m := insert.FindStringSubmatch(sql); m != nil {
		t := state[strings.ToLower(m[1])]
		if t == nil {
			return fail("unknown table")
		}

		groups, err := split(m[2])
		if err != nil {
			return fail(err.Error())
		}
		pending := [][]any{}
		keys := map[any]bool{}
		pk := primaryIndex(t)
		for n := 0; n < t.rowCount(); n++ {
			row := t.rowAt(n)
			if pk >= 0 {
				keys[row[pk]] = true
			}
		}
		for _, group := range groups {
			if len(group) < 2 || group[0] != '(' || group[len(group)-1] != ')' {
				return fail("expected row parentheses")
			}
			parts, err := split(group[1 : len(group)-1])
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
			if pk >= 0 {
				if keys[row[pk]] {
					return fail("duplicate primary key")
				}
				keys[row[pk]] = true
			}
			pending = append(pending, row)
		}
		for _, row := range pending {
			t.appendRow(row)
		}
		return Result{Affected: len(pending)}, true, nil
	}
	if m := updateRE.FindStringSubmatch(sql); m != nil {
		t := state[strings.ToLower(m[1])]
		if t == nil {
			return fail("unknown table")
		}
		where := keyword(m[2], " WHERE ")
		if where < 0 {
			return fail("UPDATE requires WHERE")
		}
		assignments, clause := strings.TrimSpace(m[2][:where]), strings.TrimSpace(m[2][where+7:])
		if clause == "" {
			return fail("UPDATE requires predicates")
		}
		preds, err := parseWhere(t, clause)
		if err != nil {
			return fail(err.Error())
		}
		parts, err := split(assignments)
		if err != nil {
			return fail(err.Error())
		}
		values := map[int]any{}
		for _, part := range parts {
			pair := strings.SplitN(part, "=", 2)
			if len(pair) != 2 {
				return fail("expected assignment")
			}
			idx := columnIndex(t, strings.TrimSpace(pair[0]))
			if idx < 0 {
				return fail("unknown column")
			}
			if t.Columns[idx].Primary {
				return fail("primary key updates unsupported")
			}
			if _, ok := values[idx]; ok {
				return fail("duplicate assignment")
			}
			v, err := parseValue(t.Columns[idx], strings.TrimSpace(pair[1]))
			if err != nil {
				return fail(err.Error())
			}
			values[idx] = v
		}
		count := 0
		for n := 0; n < t.rowCount(); n++ {
			row := t.rowAt(n)
			if matches(row, preds) {
				row = append([]any(nil), row...)
				for idx, v := range values {
					row[idx] = v
				}
				t.replaceRow(n, row)
				count++
			}
		}
		return Result{Affected: count}, true, nil
	}
	if m := deleteRE.FindStringSubmatch(sql); m != nil {
		t := state[strings.ToLower(m[1])]
		if t == nil {
			return fail("unknown table")
		}
		preds, err := parseWhere(t, m[2])
		if err != nil {
			return fail(err.Error())
		}
		kept := [][]any{}
		keptRefs := []pageRef{}
		count := 0
		for n := 0; n < t.rowCount(); n++ {
			row := t.rowAt(n)
			if matches(row, preds) {
				count++
			} else {
				if t.pages != nil {
					keptRefs = append(keptRefs, t.refs[n])
				} else {
					kept = append(kept, row)
				}
			}
		}
		t.Rows = kept
		t.refs = keptRefs
		return Result{Affected: count}, true, nil
	}

	if m := remove.FindStringSubmatch(sql); m != nil {
		n := strings.ToLower(m[2])
		t := state[n]
		if t == nil {
			return fail("unknown table")
		}
		count := t.rowCount()
		if strings.EqualFold(m[1], "DROP TABLE") {
			delete(state, n)
		} else {
			t.Rows = nil
			t.refs = nil
		}
		return Result{Affected: count}, true, nil
	}
	if strings.HasPrefix(strings.ToUpper(sql), "SELECT ") {
		r, err := selectRows(state, sql, ann)
		return r, false, err
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

var updateRE = regexp.MustCompile(`(?is)^UPDATE\s+(\w+)\s+SET\s+(.+)$`)
var deleteRE = regexp.MustCompile(`(?is)^DELETE FROM (\w+)(?: WHERE (.+))?$`)

func columnIndex(t *Table, name string) int {
	for i, c := range t.Columns {
		if c.Name == strings.ToLower(name) {
			return i
		}
	}
	return -1
}
func primaryIndex(t *Table) int {
	for i, c := range t.Columns {
		if c.Primary {
			return i
		}
	}
	return -1
}
func matches(row []any, preds []predicate) bool {
	for _, p := range preds {
		c := compare(row[p.index], p.value)
		switch p.op {
		case "=":
			if c != 0 {
				return false
			}
		case "<":
			if c >= 0 {
				return false
			}
		case "<=":
			if c > 0 {
				return false
			}
		case ">":
			if c <= 0 {
				return false
			}
		case ">=":
			if c < 0 {
				return false
			}
		}
	}
	return true
}
func compare(a, b any) int {
	switch x := a.(type) {
	case int64:
		y := b.(int64)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	case string:
		return strings.Compare(x, b.(string))
	case float64:
		y := b.(float64)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	case float32:
		y := b.(float32)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// Collapse SQL whitespace outside strings, preserving literal bytes.
func normalizeSQL(s string) string {
	var b strings.Builder
	quoted, space := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !quoted && (c == ' ' || c == '\n' || c == '\r' || c == '\t') {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
		if c == '\'' {
			if quoted && i+1 < len(s) && s[i+1] == '\'' {
				i++
				b.WriteByte(s[i])
			} else {
				quoted = !quoted
			}
		}
	}
	return b.String()
}
