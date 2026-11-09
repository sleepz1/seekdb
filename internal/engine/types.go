package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"minivectordb/internal/sqlparse"
)

// Kind is the runtime type of a Value.
type Kind uint8

const (
	KNull Kind = iota
	KInt
	KStr
	KFloat
	KVec
	KBool
)

// Value is a runtime value. Only the field matching K is meaningful.
type Value struct {
	K Kind
	I int64
	F float64
	S string
	V []float32
}

func IntVal(i int64) Value     { return Value{K: KInt, I: i} }
func StrVal(s string) Value    { return Value{K: KStr, S: s} }
func FloatVal(f float64) Value { return Value{K: KFloat, F: f} }
func VecVal(v []float32) Value { return Value{K: KVec, V: v} }
func BoolVal(b bool) Value {
	if b {
		return Value{K: KBool, I: 1}
	}
	return Value{K: KBool}
}

// Column is a column of a table schema.
type Column struct {
	Name  string
	Type  sqlparse.ColType
	Param int
}

// Schema describes a table.
type Schema struct {
	Name    string
	Columns []Column
	PK      int // index of the primary key column
	VecCol  int // index of the vector column, -1 if none
}

func (s *Schema) colIndex(name string) int {
	for i, c := range s.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// Row is a decoded row; values are in schema column order.
type Row []Value

// approxSize estimates the in-memory footprint of a row for cache accounting.
func (r Row) approxSize() int64 {
	n := int64(24 + len(r)*int(valueSize))
	for _, v := range r {
		switch v.K {
		case KStr:
			n += int64(len(v.S))
		case KVec:
			n += int64(len(v.V) * 4)
		}
	}
	return n
}

const valueSize = 64

// encodeRow appends the binary encoding of r to buf.
func encodeRow(buf []byte, s *Schema, r Row) []byte {
	for i, c := range s.Columns {
		v := r[i]
		switch c.Type {
		case sqlparse.TypeBigint:
			buf = binary.LittleEndian.AppendUint64(buf, uint64(v.I))
		case sqlparse.TypeVarchar:
			buf = binary.AppendUvarint(buf, uint64(len(v.S)))
			buf = append(buf, v.S...)
		case sqlparse.TypeVector:
			for _, f := range v.V {
				buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
			}
		}
	}
	return buf
}

func decodeRow(s *Schema, b []byte) (Row, error) {
	r := make(Row, len(s.Columns))
	p := 0
	for i, c := range s.Columns {
		switch c.Type {
		case sqlparse.TypeBigint:
			if p+8 > len(b) {
				return nil, errCorrupt
			}
			r[i] = IntVal(int64(binary.LittleEndian.Uint64(b[p:])))
			p += 8
		case sqlparse.TypeVarchar:
			n, k := binary.Uvarint(b[p:])
			if k <= 0 || p+k+int(n) > len(b) {
				return nil, errCorrupt
			}
			p += k
			r[i] = StrVal(string(b[p : p+int(n)]))
			p += int(n)
		case sqlparse.TypeVector:
			if p+4*c.Param > len(b) {
				return nil, errCorrupt
			}
			v := make([]float32, c.Param)
			for j := range v {
				v[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[p:]))
				p += 4
			}
			r[i] = VecVal(v)
		}
	}
	if p != len(b) {
		return nil, errCorrupt
	}
	return r, nil
}

var errCorrupt = fmt.Errorf("corrupt row encoding")

// parseVector parses a vector literal given as element texts and validates it
// against dim (dim <= 0 skips the dimension check).
func parseVectorElems(elems []string, dim int) ([]float32, error) {
	if dim > 0 && len(elems) != dim {
		return nil, newErr(ErrVectorDim, "vector dimension mismatch: expected %d, got %d", dim, len(elems))
	}
	if len(elems) == 0 {
		return nil, newErr(ErrVectorValue, "vector must not be empty")
	}
	out := make([]float32, len(elems))
	for i, e := range elems {
		f, err := parseFloat32(strings.TrimSpace(e))
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

func parseFloat32(s string) (float32, error) {
	if s == "" {
		return 0, newErr(ErrVectorValue, "invalid vector element ''")
	}
	// Only plain decimal / exponent notation is accepted; strconv would also
	// accept NaN, Inf, hex floats and underscores.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
			return 0, newErr(ErrVectorValue, "invalid vector element '%s'", s)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return 0, newErr(ErrVectorValue, "vector element out of float32 range '%s'", s)
		}
		return 0, newErr(ErrVectorValue, "invalid vector element '%s'", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, newErr(ErrVectorValue, "vector element must be finite '%s'", s)
	}
	f32 := float32(f)
	if math.IsInf(float64(f32), 0) {
		return 0, newErr(ErrVectorValue, "vector element out of float32 range '%s'", s)
	}
	return f32, nil
}

// parseVectorText parses "[1, 2, 3]".
func parseVectorText(s string, dim int) ([]float32, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, newErr(ErrVectorValue, "invalid vector literal '%s'", s)
	}
	inner := strings.TrimSpace(t[1 : len(t)-1])
	if inner == "" {
		return parseVectorElems(nil, dim)
	}
	return parseVectorElems(strings.Split(inner, ","), dim)
}

// FormatVector renders a vector in literal form, e.g. [1,2.5].
func FormatVector(v []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// compareValues compares two non-null scalar values of compatible kinds.
func compareValues(a, b Value) (int, error) {
	switch {
	case a.K == KInt && b.K == KInt:
		switch {
		case a.I < b.I:
			return -1, nil
		case a.I > b.I:
			return 1, nil
		}
		return 0, nil
	case a.K == KStr && b.K == KStr:
		// Go string comparison on valid UTF-8 is byte order, which equals
		// Unicode code point order.
		return strings.Compare(a.S, b.S), nil
	case isNumeric(a) && isNumeric(b):
		fa, fb := numF(a), numF(b)
		switch {
		case fa < fb:
			return -1, nil
		case fa > fb:
			return 1, nil
		}
		// int vs float equality: exact check to avoid precision loss
		if a.K == KInt && b.K == KFloat && float64(a.I) == b.F && int64(b.F) != a.I {
			return -1, nil
		}
		return 0, nil
	}
	return 0, newErr(ErrTypeMismatch, "cannot compare %s with %s", kindName(a.K), kindName(b.K))
}

func isNumeric(v Value) bool { return v.K == KInt || v.K == KFloat }

func numF(v Value) float64 {
	if v.K == KInt {
		return float64(v.I)
	}
	return v.F
}

func kindName(k Kind) string {
	switch k {
	case KInt:
		return "BIGINT"
	case KStr:
		return "VARCHAR"
	case KFloat:
		return "DOUBLE"
	case KVec:
		return "VECTOR"
	case KBool:
		return "BOOLEAN"
	}
	return "NULL"
}
