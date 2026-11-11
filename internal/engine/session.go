package engine

import (
	"container/heap"
	"sort"
	"strings"

	"minivectordb/internal/sqlparse"
)

// Result is the outcome of one statement.
type Result struct {
	IsQuery  bool
	Columns  []string
	Rows     [][]Value
	Affected int64
}

// Session is the single client session. It holds at most one explicit
// transaction whose changes live in memory until COMMIT.
type Session struct {
	e      *Engine
	inTxn  bool
	txn    map[string]map[int64]change // lower table name -> pk -> change
	Mode   string                      // "exact" or "ann"
	closed bool
}

// NewSession creates a session with the given default search mode.
func (e *Engine) NewSession(mode string) *Session {
	if mode != "ann" {
		mode = "exact"
	}
	return &Session{e: e, Mode: mode}
}

// InTxn reports whether an explicit transaction is open.
func (s *Session) InTxn() bool { return s.inTxn }

// Close rolls back any open transaction.
func (s *Session) Close() {
	s.inTxn = false
	s.txn = nil
	s.closed = true
}

// ExecSQL parses and executes one statement.
func (s *Session) ExecSQL(sql string) (*Result, error) {
	tuneGCEvery()
	st, err := sqlparse.Parse(sql)
	if err != nil {
		return nil, AsSQLError(err)
	}
	res, err := s.Exec(st)
	if err != nil {
		return nil, AsSQLError(err)
	}
	return res, nil
}

// Exec executes a parsed statement.
func (s *Session) Exec(st sqlparse.Stmt) (*Result, error) {
	switch x := st.(type) {
	case *sqlparse.Begin:
		if s.inTxn {
			return nil, newErr(ErrTxnState, "a transaction is already active; nested transactions are not supported")
		}
		s.inTxn = true
		s.txn = make(map[string]map[int64]change)
		return &Result{}, nil
	case *sqlparse.Commit:
		return &Result{}, s.commit()
	case *sqlparse.Rollback:
		s.inTxn = false
		s.txn = nil
		return &Result{}, nil
	case *sqlparse.CreateTable, *sqlparse.DropTable, *sqlparse.CreateIndex, *sqlparse.DropIndex:
		// DDL implicitly commits an open transaction (MySQL semantics).
		if err := s.commit(); err != nil {
			return nil, err
		}
		return s.execDDL(st)
	case *sqlparse.Insert:
		return s.write(x.Table, func(t *Table, sc map[int64]change) (int64, error) { return s.insert(t, x, sc) })
	case *sqlparse.Update:
		return s.write(x.Table, func(t *Table, sc map[int64]change) (int64, error) { return s.update(t, x, sc) })
	case *sqlparse.Delete:
		return s.write(x.Table, func(t *Table, sc map[int64]change) (int64, error) { return s.delete(t, x, sc) })
	case *sqlparse.Select:
		return s.selectStmt(x)
	case *sqlparse.Set:
		return s.set(x)
	case *sqlparse.ShowTables:
		names := make([]string, 0, len(s.e.tables))
		for _, t := range s.e.tables {
			names = append(names, t.schema.Name)
		}
		sort.Strings(names)
		res := &Result{IsQuery: true, Columns: []string{"Tables"}}
		for _, n := range names {
			res.Rows = append(res.Rows, []Value{StrVal(n)})
		}
		return res, nil
	}
	return nil, newErr(ErrUnsupported, "unsupported statement")
}

func (s *Session) set(x *sqlparse.Set) (*Result, error) {
	if x.Name == "names" {
		return &Result{}, nil
	}
	if strings.Contains(x.Name, "mode") || strings.Contains(x.Name, "search") || strings.Contains(x.Name, "ann") {
		switch x.Value {
		case "ann", "approx", "approximate":
			s.Mode = "ann"
			return &Result{}, nil
		case "exact":
			s.Mode = "exact"
			return &Result{}, nil
		}
		return nil, newErr(ErrUnsupported, "unknown search mode '%s'", x.Value)
	}
	if x.Name == "autocommit" && (x.Value == "1" || x.Value == "on") {
		return &Result{}, nil
	}
	return nil, newErr(ErrUnsupported, "unsupported variable '%s'", x.Name)
}

func (s *Session) commit() error {
	if !s.inTxn {
		return nil
	}
	changes := s.txn
	s.inTxn = false
	s.txn = nil
	return s.e.commitChanges(changes)
}

// write runs a data modification against a statement-level change set. On
// success the changes join the open transaction or are committed at once
// (autocommit). On failure they are discarded, so a failed statement never
// leaves partial effects.
func (s *Session) write(table string, fn func(*Table, map[int64]change) (int64, error)) (*Result, error) {
	t, err := s.e.table(table)
	if err != nil {
		return nil, err
	}
	sc := make(map[int64]change)
	n, err := fn(t, sc)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(t.schema.Name)
	if s.inTxn {
		m := s.txn[key]
		if m == nil {
			m = make(map[int64]change)
			s.txn[key] = m
		}
		for pk, c := range sc {
			m[pk] = c
		}
		return &Result{Affected: n}, nil
	}
	if err := s.e.commitChanges(map[string]map[int64]change{key: sc}); err != nil {
		return nil, err
	}
	return &Result{Affected: n}, nil
}

// ---- reading through the transaction layers ----------------------------

func (s *Session) txnLayer(t *Table) map[int64]change {
	if !s.inTxn {
		return nil
	}
	return s.txn[strings.ToLower(t.schema.Name)]
}

// get returns the visible row for pk, consulting the statement layer, the
// transaction layer and then committed state.
func (s *Session) get(t *Table, pk int64, sc map[int64]change) (Row, bool, error) {
	if c, ok := sc[pk]; ok {
		return c.row, !c.deleted, nil
	}
	if c, ok := s.txnLayer(t)[pk]; ok {
		return c.row, !c.deleted, nil
	}
	si, ok := t.index[pk]
	if !ok {
		return nil, false, nil
	}
	r, err := s.e.loadRow(t, si)
	return r, err == nil, err
}

// scan visits every visible row exactly once (in no particular order).
func (s *Session) scan(t *Table, sc map[int64]change, fn func(pk int64, r Row) error) error {
	tl := s.txnLayer(t)
	var skip func(int64) bool
	if len(tl) > 0 || len(sc) > 0 {
		skip = func(pk int64) bool {
			if _, ok := sc[pk]; ok {
				return true
			}
			_, ok := tl[pk]
			return ok
		}
	}
	if err := s.e.scanCommitted(t, skip, fn); err != nil {
		return err
	}
	for pk, c := range tl {
		if _, ok := sc[pk]; ok || c.deleted {
			continue
		}
		if err := fn(pk, c.row); err != nil {
			return err
		}
	}
	for pk, c := range sc {
		if c.deleted {
			continue
		}
		if err := fn(pk, c.row); err != nil {
			return err
		}
	}
	return nil
}

// matching visits visible rows satisfying where, using a primary-key point
// lookup when the predicate pins the key.
func (s *Session) matching(t *Table, where sqlparse.Expr, sc map[int64]change, fn func(pk int64, r Row) error) error {
	var filter cexpr
	if where != nil {
		var err error
		filter, err = (&compiler{s: t.schema}).compile(where)
		if err != nil {
			return err
		}
	}
	check := func(pk int64, r Row) error {
		if filter != nil {
			v, err := filter.eval(r)
			if err != nil {
				return err
			}
			ok, err := truth(v)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		return fn(pk, r)
	}
	if where != nil {
		if pk, ok := pkEquality(where, t.schema); ok {
			r, found, err := s.get(t, pk, sc)
			if err != nil || !found {
				return err
			}
			return check(pk, r)
		}
	}
	return s.scan(t, sc, check)
}

// ---- INSERT / UPDATE / DELETE -------------------------------------------

// cellValue converts one INSERT literal to the value stored in column col.
func cellValue(lit *sqlparse.Literal, col Column) (Value, error) {
	if lit.Kind == sqlparse.LitVector && col.Type == sqlparse.TypeVector {
		vec, err := parseVectorElems(lit.Vec, col.Param)
		if err != nil {
			return Value{}, err
		}
		return VecVal(vec), nil
	}
	lv, err := literalValue(lit)
	if err != nil {
		return Value{}, err
	}
	return coerce(col, lv)
}

func (s *Session) insert(t *Table, x *sqlparse.Insert, sc map[int64]change) (int64, error) {
	sch := t.schema
	order := make([]int, len(sch.Columns))
	if x.Columns == nil {
		for i := range order {
			order[i] = i
		}
	} else {
		if len(x.Columns) != len(sch.Columns) {
			seen := map[int]bool{}
			for _, c := range x.Columns {
				if i := sch.colIndex(c); i >= 0 {
					seen[i] = true
				} else {
					return 0, newErr(ErrNoColumn, "Unknown column '%s' in 'field list'", c)
				}
			}
			for i, c := range sch.Columns {
				if !seen[i] {
					return 0, newErr(ErrNullValue, "Field '%s' doesn't have a default value", c.Name)
				}
			}
		}
		seen := map[int]bool{}
		for j, c := range x.Columns {
			i := sch.colIndex(c)
			if i < 0 {
				return 0, newErr(ErrNoColumn, "Unknown column '%s' in 'field list'", c)
			}
			if seen[i] {
				return 0, newErr(ErrDupColumn, "Column '%s' specified twice", c)
			}
			seen[i] = true
			order[j] = i
		}
	}
	// Convert the vector values of large multi-row INSERTs in parallel up
	// front; results and errors are consumed below in the original order, so
	// the statement behaves exactly as with sequential conversion.
	type cell struct {
		v   Value
		err error
		ok  bool
	}
	var pre [][]cell
	if Workers > 1 && len(x.Rows) >= 32 {
		pre = make([][]cell, len(x.Rows))
		parallelFor(len(x.Rows), func(lo, hi int) {
			for ri := lo; ri < hi; ri++ {
				vals := x.Rows[ri]
				if len(vals) != len(sch.Columns) {
					continue
				}
				out := make([]cell, len(vals))
				for j, ex := range vals {
					lit, ok := ex.(*sqlparse.Literal)
					col := sch.Columns[order[j]]
					if !ok || lit.Kind == sqlparse.LitNull || col.Type != sqlparse.TypeVector {
						continue
					}
					out[j].v, out[j].err = cellValue(lit, col)
					out[j].ok = true
				}
				pre[ri] = out
			}
		})
	}
	for ri, vals := range x.Rows {
		if len(vals) != len(sch.Columns) {
			return 0, newErr(ErrValueCount, "Column count doesn't match value count at row %d", ri+1)
		}
		row := make(Row, len(sch.Columns))
		for j, ex := range vals {
			lit, ok := ex.(*sqlparse.Literal)
			if !ok {
				return 0, newErr(ErrUnsupported, "INSERT values must be literals")
			}
			col := sch.Columns[order[j]]
			if lit.Kind == sqlparse.LitNull {
				return 0, newErr(ErrNullValue, "Column '%s' cannot be null", col.Name)
			}
			var v Value
			var err error
			if pre != nil && pre[ri][j].ok {
				v, err = pre[ri][j].v, pre[ri][j].err
			} else {
				v, err = cellValue(lit, col)
			}
			if err != nil {
				return 0, err
			}
			row[order[j]] = v
		}
		pk := row[sch.PK].I
		if _, exists, err := s.get(t, pk, sc); err != nil {
			return 0, err
		} else if exists {
			return 0, newErr(ErrDupKey, "Duplicate entry '%d' for key 'PRIMARY'", pk)
		}
		sc[pk] = change{row: row}
	}
	return int64(len(x.Rows)), nil
}

func (s *Session) update(t *Table, x *sqlparse.Update, sc map[int64]change) (int64, error) {
	sch := t.schema
	type setter struct {
		idx int
		ex  cexpr
		lit *sqlparse.Literal
	}
	comp := &compiler{s: sch}
	var sets []setter
	seen := map[int]bool{}
	for _, a := range x.Sets {
		i := sch.colIndex(a.Column)
		if i < 0 {
			return 0, newErr(ErrNoColumn, "Unknown column '%s' in 'field list'", a.Column)
		}
		if i == sch.PK {
			return 0, newErr(ErrPKUpdate, "updating the primary key is not supported")
		}
		if seen[i] {
			return 0, newErr(ErrDupColumn, "Column '%s' specified twice", a.Column)
		}
		seen[i] = true
		st := setter{idx: i}
		if lit, ok := a.Value.(*sqlparse.Literal); ok {
			if lit.Kind == sqlparse.LitNull {
				return 0, newErr(ErrNullValue, "Column '%s' cannot be null", sch.Columns[i].Name)
			}
			st.lit = lit
			// Validate constants up front so errors do not depend on data.
			if _, err := s.assignValue(sch.Columns[i], lit, nil, nil); err != nil {
				return 0, err
			}
		} else {
			ex, err := comp.compile(a.Value)
			if err != nil {
				return 0, err
			}
			st.ex = ex
		}
		sets = append(sets, st)
	}
	type hit struct {
		pk  int64
		row Row
	}
	var hits []hit
	err := s.matching(t, x.Where, sc, func(pk int64, r Row) error {
		hits = append(hits, hit{pk, r})
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, h := range hits {
		nr := make(Row, len(h.row))
		copy(nr, h.row)
		for _, st := range sets {
			v, err := s.assignValue(sch.Columns[st.idx], st.lit, st.ex, h.row)
			if err != nil {
				return 0, err
			}
			nr[st.idx] = v
		}
		sc[h.pk] = change{row: nr}
	}
	return int64(len(hits)), nil
}

func (s *Session) assignValue(col Column, lit *sqlparse.Literal, ex cexpr, old Row) (Value, error) {
	if lit != nil {
		if lit.Kind == sqlparse.LitVector && col.Type == sqlparse.TypeVector {
			vec, err := parseVectorElems(lit.Vec, col.Param)
			if err != nil {
				return Value{}, err
			}
			return VecVal(vec), nil
		}
		lv, err := literalValue(lit)
		if err != nil {
			return Value{}, err
		}
		return coerce(col, lv)
	}
	v, err := ex.eval(old)
	if err != nil {
		return Value{}, err
	}
	if v.K == KBool {
		v = IntVal(v.I)
	}
	return coerce(col, v)
}

func (s *Session) delete(t *Table, x *sqlparse.Delete, sc map[int64]change) (int64, error) {
	var pks []int64
	err := s.matching(t, x.Where, sc, func(pk int64, r Row) error {
		pks = append(pks, pk)
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, pk := range pks {
		sc[pk] = change{deleted: true}
	}
	return int64(len(pks)), nil
}

// ---- SELECT -------------------------------------------------------------

type outCol struct {
	name string
	ex   cexpr
}

type orderKey struct {
	ex   cexpr
	desc bool
}

type candidate struct {
	pk   int64
	row  Row
	keys []Value
}

func (s *Session) selectStmt(x *sqlparse.Select) (*Result, error) {
	var t *Table
	var sch *Schema
	if x.Table != "" {
		var err error
		if t, err = s.e.table(x.Table); err != nil {
			return nil, err
		}
		sch = t.schema
	}
	comp := &compiler{s: sch}

	var cols []outCol
	for _, it := range x.Items {
		if it.Star {
			if sch == nil {
				return nil, newErr(ErrParse, "SELECT * requires a FROM clause")
			}
			for i, c := range sch.Columns {
				cols = append(cols, outCol{name: c.Name, ex: colE{i}})
			}
			continue
		}
		ex, err := comp.compile(it.Expr)
		if err != nil {
			return nil, err
		}
		name := it.Alias
		if name == "" {
			if cr, ok := it.Expr.(*sqlparse.ColRef); ok {
				name = cr.Name
			} else {
				name = it.Text
			}
		}
		cols = append(cols, outCol{name: name, ex: ex})
	}

	var keys []orderKey
	for _, o := range x.Order {
		ex, err := s.orderExpr(o.Expr, x.Items, cols, comp)
		if err != nil {
			return nil, err
		}
		if k := comp.staticKind(ex); k == KVec {
			return nil, newErr(ErrTypeMismatch, "cannot ORDER BY a vector value")
		}
		keys = append(keys, orderKey{ex: ex, desc: o.Desc})
	}

	res := &Result{IsQuery: true, Columns: make([]string, len(cols))}
	for i, c := range cols {
		res.Columns[i] = c.name
	}

	if t == nil {
		// SELECT without FROM: a single row of constants.
		if x.Where != nil || x.Limit == 0 {
			if x.Limit == 0 {
				return res, nil
			}
		}
		out := make([]Value, len(cols))
		for i, c := range cols {
			v, err := c.ex.eval(nil)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		res.Rows = append(res.Rows, out)
		return res, nil
	}

	limit := x.Limit
	if limit == 0 {
		// Still validate the predicate by compiling it.
		if x.Where != nil {
			if _, err := comp.compile(x.Where); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	want := int64(-1)
	if limit > 0 {
		want = x.Offset + limit
	}

	var picked []candidate
	if l2, ok := singleL2Key(keys); ok && want > 0 {
		var err error
		if picked, err = s.topKL2(t, x.Where, l2, int(want)); err != nil {
			return nil, err
		}
	} else if len(keys) > 0 && want > 0 {
		h := &candHeap{keys: keys}
		var evalErr error
		err := s.matching(t, x.Where, nil, func(pk int64, r Row) error {
			kv := make([]Value, len(keys))
			for i, k := range keys {
				v, err := k.ex.eval(r)
				if err != nil {
					return err
				}
				kv[i] = v
			}
			c := candidate{pk: pk, row: r, keys: kv}
			if int64(h.Len()) < want {
				heap.Push(h, c)
				return h.err
			}
			// h[0] is the worst kept candidate.
			if less(keys, c, h.items[0], &evalErr) {
				h.items[0] = c
				heap.Fix(h, 0)
			}
			if evalErr != nil {
				return evalErr
			}
			return h.err
		})
		if err != nil {
			return nil, err
		}
		picked = h.items
		var sortErr error
		sort.Slice(picked, func(i, j int) bool { return less(keys, picked[i], picked[j], &sortErr) })
		if sortErr != nil {
			return nil, sortErr
		}
	} else {
		err := s.matching(t, x.Where, nil, func(pk int64, r Row) error {
			var kv []Value
			if len(keys) > 0 {
				kv = make([]Value, len(keys))
				for i, k := range keys {
					v, err := k.ex.eval(r)
					if err != nil {
						return err
					}
					kv[i] = v
				}
			}
			picked = append(picked, candidate{pk: pk, row: r, keys: kv})
			return nil
		})
		if err != nil {
			return nil, err
		}
		var sortErr error
		sort.Slice(picked, func(i, j int) bool { return less(keys, picked[i], picked[j], &sortErr) })
		if sortErr != nil {
			return nil, sortErr
		}
	}

	start := x.Offset
	if start > int64(len(picked)) {
		start = int64(len(picked))
	}
	end := int64(len(picked))
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	for _, c := range picked[start:end] {
		out := make([]Value, len(cols))
		for i, oc := range cols {
			v, err := oc.ex.eval(c.row)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		res.Rows = append(res.Rows, out)
	}
	return res, nil
}

// orderExpr resolves an ORDER BY expression: select-list aliases first, then
// 1-based ordinals, then ordinary expressions over the table.
func (s *Session) orderExpr(e sqlparse.Expr, items []sqlparse.SelectItem, cols []outCol, comp *compiler) (cexpr, error) {
	if cr, ok := e.(*sqlparse.ColRef); ok {
		for i, it := range items {
			if !it.Star && it.Alias != "" && strings.EqualFold(it.Alias, cr.Name) {
				return s.itemExpr(i, items, cols), nil
			}
		}
	}
	if lit, ok := e.(*sqlparse.Literal); ok && lit.Kind == sqlparse.LitInt {
		v, err := literalValue(lit)
		if err != nil {
			return nil, err
		}
		if v.I < 1 || v.I > int64(len(cols)) {
			return nil, newErr(ErrNoColumn, "Unknown column '%s' in 'order clause'", lit.Text)
		}
		return cols[v.I-1].ex, nil
	}
	return comp.compile(e)
}

// itemExpr returns the compiled expression of select item i, accounting for
// '*' expansions before it.
func (s *Session) itemExpr(i int, items []sqlparse.SelectItem, cols []outCol) cexpr {
	pos := 0
	for j := 0; j < i; j++ {
		if items[j].Star {
			pos += len(cols) - countNonStar(items)
		} else {
			pos++
		}
	}
	return cols[pos].ex
}

func countNonStar(items []sqlparse.SelectItem) int {
	n := 0
	for _, it := range items {
		if !it.Star {
			n++
		}
	}
	return n
}

// less orders candidates by the ORDER BY keys and then by primary key
// ascending, which is the required tie-break rule (also with DESC keys).
func less(keys []orderKey, a, b candidate, errp *error) bool {
	for i, k := range keys {
		d, err := compareValues(a.keys[i], b.keys[i])
		if err != nil {
			if *errp == nil {
				*errp = err
			}
			return false
		}
		if d != 0 {
			if k.desc {
				return d > 0
			}
			return d < 0
		}
	}
	return a.pk < b.pk
}

// candHeap is a max-heap by output order: the root is the candidate that
// would be emitted last, so it is the one to evict.
type candHeap struct {
	items []candidate
	keys  []orderKey
	err   error
}

func (h *candHeap) Len() int           { return len(h.items) }
func (h *candHeap) Less(i, j int) bool { return less(h.keys, h.items[j], h.items[i], &h.err) }
func (h *candHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *candHeap) Push(x any)         { h.items = append(h.items, x.(candidate)) }
func (h *candHeap) Pop() any {
	n := len(h.items)
	x := h.items[n-1]
	h.items = h.items[:n-1]
	return x
}
