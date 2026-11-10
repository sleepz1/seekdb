package engine

import (
	"math"
	"strconv"
	"strings"

	"minivectordb/internal/sqlparse"
)

// cexpr is a compiled expression evaluated against a row.
type cexpr interface {
	eval(r Row) (Value, error)
}

type constE struct{ v Value }
type colE struct{ idx int }
type cmpE struct {
	op   string
	l, r cexpr
}

// strEqE avoids generic Value dispatch for the common ordinary-column filter.
// It is only constructed for a statically typed VARCHAR column and string.
type strEqE struct {
	col   int
	value string
}

func (c strEqE) eval(r Row) (Value, error) {
	return BoolVal(r[c.col].S == c.value), nil
}

type andE struct{ l, r cexpr }
type orE struct{ l, r cexpr }
type notE struct{ x cexpr }

// l2E computes L2_DISTANCE between a vector column and a constant vector.
type l2E struct {
	col int
	q   []float32
}

// l2ColsE computes L2_DISTANCE between two vector expressions.
type l2ColsE struct{ a, b cexpr }

func (c constE) eval(Row) (Value, error) { return c.v, nil }
func (c colE) eval(r Row) (Value, error) { return r[c.idx], nil }

func (c cmpE) eval(r Row) (Value, error) {
	a, err := c.l.eval(r)
	if err != nil {
		return Value{}, err
	}
	b, err := c.r.eval(r)
	if err != nil {
		return Value{}, err
	}
	d, err := compareValues(a, b)
	if err != nil {
		return Value{}, err
	}
	switch c.op {
	case "=":
		return BoolVal(d == 0), nil
	case "!=":
		return BoolVal(d != 0), nil
	case "<":
		return BoolVal(d < 0), nil
	case "<=":
		return BoolVal(d <= 0), nil
	case ">":
		return BoolVal(d > 0), nil
	case ">=":
		return BoolVal(d >= 0), nil
	}
	return Value{}, newErr(ErrInternal, "bad operator %s", c.op)
}

func truth(v Value) (bool, error) {
	switch v.K {
	case KBool, KInt:
		return v.I != 0, nil
	case KFloat:
		return v.F != 0, nil
	}
	return false, newErr(ErrTypeMismatch, "%s value used as a condition", kindName(v.K))
}

func (c andE) eval(r Row) (Value, error) {
	a, err := c.l.eval(r)
	if err != nil {
		return Value{}, err
	}
	ta, err := truth(a)
	if err != nil {
		return Value{}, err
	}
	if !ta {
		return BoolVal(false), nil
	}
	b, err := c.r.eval(r)
	if err != nil {
		return Value{}, err
	}
	tb, err := truth(b)
	if err != nil {
		return Value{}, err
	}
	return BoolVal(tb), nil
}

func (c orE) eval(r Row) (Value, error) {
	a, err := c.l.eval(r)
	if err != nil {
		return Value{}, err
	}
	ta, err := truth(a)
	if err != nil {
		return Value{}, err
	}
	if ta {
		return BoolVal(true), nil
	}
	b, err := c.r.eval(r)
	if err != nil {
		return Value{}, err
	}
	tb, err := truth(b)
	if err != nil {
		return Value{}, err
	}
	return BoolVal(tb), nil
}

func (c notE) eval(r Row) (Value, error) {
	a, err := c.x.eval(r)
	if err != nil {
		return Value{}, err
	}
	t, err := truth(a)
	if err != nil {
		return Value{}, err
	}
	return BoolVal(!t), nil
}

func (c l2E) eval(r Row) (Value, error) {
	return l2Value(r[c.col].V, c.q)
}

func (c l2ColsE) eval(r Row) (Value, error) {
	a, err := c.a.eval(r)
	if err != nil {
		return Value{}, err
	}
	b, err := c.b.eval(r)
	if err != nil {
		return Value{}, err
	}
	if a.K != KVec || b.K != KVec {
		return Value{}, newErr(ErrTypeMismatch, "L2_DISTANCE requires vector arguments")
	}
	if len(a.V) != len(b.V) {
		return Value{}, newErr(ErrVectorDim, "vector dimension mismatch: %d vs %d", len(a.V), len(b.V))
	}
	return l2Value(a.V, b.V)
}

func l2Value(a, b []float32) (Value, error) {
	d := math.Sqrt(l2sq(a, b))
	if math.IsInf(d, 0) || math.IsNaN(d) {
		return Value{}, newErr(ErrOutOfRange, "distance out of range")
	}
	return FloatVal(d), nil
}

// l2sq returns the squared Euclidean distance accumulated in float64.
func l2sq(a, b []float32) float64 {
	var s0, s1, s2, s3 float64
	n := len(a)
	b = b[:n]
	i := 0
	for ; i+4 <= n; i += 4 {
		d0 := float64(a[i]) - float64(b[i])
		d1 := float64(a[i+1]) - float64(b[i+1])
		d2 := float64(a[i+2]) - float64(b[i+2])
		d3 := float64(a[i+3]) - float64(b[i+3])
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	for ; i < n; i++ {
		d := float64(a[i]) - float64(b[i])
		s0 += d * d
	}
	return (s0 + s1) + (s2 + s3)
}

// l2sqBounded computes the squared distance like l2sq but stops early and
// returns full=false once the partial sum exceeds bound (partial sums only
// grow, so such a row cannot be within bound).
func l2sqBounded(a, b []float32, bound float64) (float64, bool) {
	n := len(a)
	b = b[:n]
	var s0, s1, s2, s3 float64
	i := 0
	for i+16 <= n {
		for end := i + 16; i < end; i += 4 {
			d0 := float64(a[i]) - float64(b[i])
			d1 := float64(a[i+1]) - float64(b[i+1])
			d2 := float64(a[i+2]) - float64(b[i+2])
			d3 := float64(a[i+3]) - float64(b[i+3])
			s0 += d0 * d0
			s1 += d1 * d1
			s2 += d2 * d2
			s3 += d3 * d3
		}
		if (s0+s1)+(s2+s3) > bound {
			return 0, false
		}
	}
	for ; i+4 <= n; i += 4 {
		d0 := float64(a[i]) - float64(b[i])
		d1 := float64(a[i+1]) - float64(b[i+1])
		d2 := float64(a[i+2]) - float64(b[i+2])
		d3 := float64(a[i+3]) - float64(b[i+3])
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	for ; i < n; i++ {
		d := float64(a[i]) - float64(b[i])
		s0 += d * d
	}
	// Same association order as l2sq, so the full sum is bit-identical.
	return (s0 + s1) + (s2 + s3), true
}

// compiler resolves column references against a schema.
type compiler struct {
	s *Schema // nil for SELECT without FROM
}

func (c *compiler) compile(e sqlparse.Expr) (cexpr, error) {
	switch x := e.(type) {
	case *sqlparse.Literal:
		v, err := literalValue(x)
		if err != nil {
			return nil, err
		}
		return constE{v}, nil
	case *sqlparse.ColRef:
		if c.s == nil {
			return nil, newErr(ErrNoColumn, "Unknown column '%s' in 'field list'", x.Name)
		}
		i := c.s.colIndex(x.Name)
		if i < 0 {
			return nil, newErr(ErrNoColumn, "Unknown column '%s'", x.Name)
		}
		return colE{i}, nil
	case *sqlparse.Binary:
		switch x.Op {
		case "AND", "OR":
			l, err := c.compile(x.L)
			if err != nil {
				return nil, err
			}
			r, err := c.compile(x.R)
			if err != nil {
				return nil, err
			}
			if x.Op == "AND" {
				return andE{l, r}, nil
			}
			return orE{l, r}, nil
		}
		return c.compileCmp(x)
	case *sqlparse.Not:
		inner, err := c.compile(x.X)
		if err != nil {
			return nil, err
		}
		return notE{inner}, nil
	case *sqlparse.Func:
		return c.compileFunc(x)
	}
	return nil, newErr(ErrUnsupported, "unsupported expression")
}

// compileCmp compiles a comparison, checking operand types where they are
// statically known so type errors are reported even for empty tables.
func (c *compiler) compileCmp(x *sqlparse.Binary) (cexpr, error) {
	l, err := c.compile(x.L)
	if err != nil {
		return nil, err
	}
	r, err := c.compile(x.R)
	if err != nil {
		return nil, err
	}
	lk, rk := c.staticKind(l), c.staticKind(r)
	if lk == KVec || rk == KVec {
		return nil, newErr(ErrTypeMismatch, "vectors cannot be compared with %s", x.Op)
	}
	if lk != KNull && rk != KNull {
		num := func(k Kind) bool { return k == KInt || k == KFloat }
		if !(lk == rk || num(lk) && num(rk)) {
			return nil, newErr(ErrTypeMismatch, "cannot compare %s with %s", kindName(lk), kindName(rk))
		}
	}
	if x.Op == "=" {
		if col, ok := l.(colE); ok && lk == KStr {
			if value, ok := r.(constE); ok && value.v.K == KStr {
				return strEqE{col: col.idx, value: value.v.S}, nil
			}
		}
		if col, ok := r.(colE); ok && rk == KStr {
			if value, ok := l.(constE); ok && value.v.K == KStr {
				return strEqE{col: col.idx, value: value.v.S}, nil
			}
		}
	}
	return cmpE{op: x.Op, l: l, r: r}, nil
}

// staticKind returns the value kind of a compiled expression when known
// without evaluating it; KNull means unknown.
func (c *compiler) staticKind(e cexpr) Kind {
	switch x := e.(type) {
	case constE:
		return x.v.K
	case colE:
		switch c.s.Columns[x.idx].Type {
		case sqlparse.TypeBigint:
			return KInt
		case sqlparse.TypeVarchar:
			return KStr
		case sqlparse.TypeVector:
			return KVec
		}
	case l2E, l2ColsE:
		return KFloat
	case cmpE, strEqE, andE, orE, notE:
		return KBool
	}
	return KNull
}

func (c *compiler) compileFunc(f *sqlparse.Func) (cexpr, error) {
	switch f.Name {
	case "L2_DISTANCE", "EUCLIDEAN_DISTANCE":
	default:
		return nil, newErr(ErrUnsupported, "function %s is not supported", f.Name)
	}
	if len(f.Args) != 2 {
		return nil, newErr(ErrParse, "L2_DISTANCE requires exactly 2 arguments")
	}
	colIdx, litIdx := -1, -1
	for i, a := range f.Args {
		if cr, ok := a.(*sqlparse.ColRef); ok && c.s != nil {
			if ci := c.s.colIndex(cr.Name); ci >= 0 && c.s.Columns[ci].Type == sqlparse.TypeVector {
				if colIdx < 0 {
					colIdx = ci
					continue
				}
			}
		}
		if _, ok := a.(*sqlparse.Literal); ok {
			litIdx = i
		}
	}
	if colIdx >= 0 && litIdx >= 0 {
		dim := c.s.Columns[colIdx].Param
		q, err := vectorFromLiteral(f.Args[litIdx].(*sqlparse.Literal), dim)
		if err != nil {
			return nil, err
		}
		return l2E{col: colIdx, q: q}, nil
	}
	a, err := c.compileVecArg(f.Args[0])
	if err != nil {
		return nil, err
	}
	b, err := c.compileVecArg(f.Args[1])
	if err != nil {
		return nil, err
	}
	return l2ColsE{a, b}, nil
}

func (c *compiler) compileVecArg(e sqlparse.Expr) (cexpr, error) {
	if lit, ok := e.(*sqlparse.Literal); ok {
		v, err := vectorFromLiteral(lit, 0)
		if err != nil {
			return nil, err
		}
		return constE{VecVal(v)}, nil
	}
	ce, err := c.compile(e)
	if err != nil {
		return nil, err
	}
	if k := c.staticKind(ce); k != KVec && k != KNull {
		return nil, newErr(ErrTypeMismatch, "L2_DISTANCE requires vector arguments")
	}
	return ce, nil
}

func vectorFromLiteral(l *sqlparse.Literal, dim int) ([]float32, error) {
	switch l.Kind {
	case sqlparse.LitVector:
		return parseVectorElems(l.Vec, dim)
	case sqlparse.LitString:
		return parseVectorText(l.Text, dim)
	}
	return nil, newErr(ErrTypeMismatch, "expected a vector literal")
}

// literalValue converts a literal to a runtime value (vectors in brackets
// become KVec; quoted strings stay strings until coerced by context).
func literalValue(l *sqlparse.Literal) (Value, error) {
	switch l.Kind {
	case sqlparse.LitInt:
		i, err := strconv.ParseInt(l.Text, 10, 64)
		if err != nil {
			return Value{}, newErr(ErrOutOfRange, "Out of range value '%s' for BIGINT", l.Text)
		}
		return IntVal(i), nil
	case sqlparse.LitFloat:
		f, err := strconv.ParseFloat(l.Text, 64)
		if err != nil || math.IsInf(f, 0) {
			return Value{}, newErr(ErrOutOfRange, "Out of range value '%s'", l.Text)
		}
		return FloatVal(f), nil
	case sqlparse.LitString:
		return StrVal(l.Text), nil
	case sqlparse.LitVector:
		v, err := parseVectorElems(l.Vec, 0)
		if err != nil {
			return Value{}, err
		}
		return VecVal(v), nil
	case sqlparse.LitNull:
		return Value{}, newErr(ErrUnsupported, "NULL is not supported")
	}
	return Value{}, newErr(ErrInternal, "bad literal")
}

// coerce converts v to the type of column col, validating constraints.
func coerce(col Column, v Value) (Value, error) {
	switch col.Type {
	case sqlparse.TypeBigint:
		switch v.K {
		case KInt:
			return v, nil
		case KFloat:
			return Value{}, newErr(ErrTypeMismatch, "Incorrect integer value '%s' for column '%s'", strconv.FormatFloat(v.F, 'g', -1, 64), col.Name)
		case KStr:
			return Value{}, newErr(ErrTypeMismatch, "Incorrect integer value '%s' for column '%s'", v.S, col.Name)
		}
	case sqlparse.TypeVarchar:
		if v.K == KStr {
			if col.Param > 0 && runeLen(v.S) > col.Param {
				return Value{}, newErr(ErrDataTooLong, "Data too long for column '%s'", col.Name)
			}
			return v, nil
		}
		return Value{}, newErr(ErrTypeMismatch, "Incorrect string value for column '%s'", col.Name)
	case sqlparse.TypeVector:
		switch v.K {
		case KStr:
			vec, err := parseVectorText(v.S, col.Param)
			if err != nil {
				return Value{}, err
			}
			return VecVal(vec), nil
		case KVec:
			if len(v.V) != col.Param {
				return Value{}, newErr(ErrVectorDim, "vector dimension mismatch: expected %d, got %d", col.Param, len(v.V))
			}
			return v, nil
		}
		return Value{}, newErr(ErrTypeMismatch, "Incorrect vector value for column '%s'", col.Name)
	}
	return Value{}, newErr(ErrTypeMismatch, "Incorrect value for column '%s'", col.Name)
}

// pkEquality finds a top-level conjunct "pk = <int literal>" in where.
func pkEquality(where sqlparse.Expr, s *Schema) (int64, bool) {
	switch x := where.(type) {
	case *sqlparse.Binary:
		if x.Op == "AND" {
			if v, ok := pkEquality(x.L, s); ok {
				return v, true
			}
			return pkEquality(x.R, s)
		}
		if x.Op != "=" {
			return 0, false
		}
		col, lit := x.L, x.R
		if _, ok := col.(*sqlparse.ColRef); !ok {
			col, lit = x.R, x.L
		}
		cr, ok1 := col.(*sqlparse.ColRef)
		l, ok2 := lit.(*sqlparse.Literal)
		if !ok1 || !ok2 || l.Kind != sqlparse.LitInt {
			return 0, false
		}
		if !strings.EqualFold(cr.Name, s.Columns[s.PK].Name) {
			return 0, false
		}
		v, err := strconv.ParseInt(l.Text, 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
