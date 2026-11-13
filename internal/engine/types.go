package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
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
			buf = appendVector(buf, v.V)
		}
	}
	return buf
}

// Vector encoding: one tag byte, then
//
//	tag 0:      dim little-endian float32 bit patterns (any vector);
//	tag 1+p:    dim 24-bit little-endian signed integers k, value =
//	            float32(float64(k) / 10^p), p in 0..7.
//
// The scaled form is used only when every element round-trips bit-exactly
// through it (checked at encode time), so decoding always yields the stored
// float32 values. Typical data with a few decimal digits (e.g. "0.123456")
// shrinks from 4 to 3 bytes per element, cutting log size and disk I/O.
func appendVector(buf []byte, v []float32) []byte {
	if p, ok := decimalScale(v); ok {
		// Bit-packed scaled integers: tag 16+p, width byte w, then the
		// values k + 2^(w-1) as w-bit little-endian fields. "0.123456"-style
		// data needs 21 bits instead of 24 (or 32 for raw float32).
		sc := exactPow10[p]
		var maxAbs uint32
		for _, f := range v {
			k := int32(math.Round(float64(f) * sc))
			if k < 0 {
				k = -k
			}
			if uint32(k) > maxAbs {
				maxAbs = uint32(k)
			}
		}
		w := uint(bits.Len32(maxAbs)) + 1
		buf = append(buf, byte(16+p), byte(w))
		off := uint64(1) << (w - 1)
		var acc uint64
		var nacc uint
		for _, f := range v {
			k := int64(math.Round(float64(f) * sc))
			acc |= uint64(k+int64(off)) << nacc
			nacc += w
			for nacc >= 8 {
				buf = append(buf, byte(acc))
				acc >>= 8
				nacc -= 8
			}
		}
		if nacc > 0 {
			buf = append(buf, byte(acc))
		}
		return buf
	}
	buf = append(buf, 0)
	for _, f := range v {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return buf
}

// decimalScale finds the smallest p (0..7) such that every element equals
// float32(k/10^p) bit-for-bit for an integer |k| < 2^23.
func decimalScale(v []float32) (int, bool) {
	if len(v) == 0 {
		return 0, false
	}
	p := 0
	for p <= 7 && !scaledExact(v[0], p) {
		p++
	}
	for p <= 7 {
		ok := true
		for _, f := range v {
			if !scaledExact(f, p) {
				ok = false
				break
			}
		}
		if ok {
			return p, true
		}
		p++
	}
	return 0, false
}

func scaledExact(f float32, p int) bool {
	if math.Float32bits(f) == 1<<31 {
		return false // -0 has no integer representation
	}
	sc := exactPow10[p]
	k := math.Round(float64(f) * sc)
	if k >= 1<<23 || k <= -(1<<23) {
		return false
	}
	return math.Float32bits(float32(k/sc)) == math.Float32bits(f)
}

// decodeVector fills v from b and returns the number of bytes consumed.
func decodeVector(v []float32, b []byte) (int, error) {
	if len(b) < 1 {
		return 0, errCorrupt
	}
	tag := b[0]
	switch {
	case tag == 0:
		if 1+4*len(v) > len(b) {
			return 0, errCorrupt
		}
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[1+4*j:]))
		}
		return 1 + 4*len(v), nil
	case tag <= 8:
		if 1+3*len(v) > len(b) {
			return 0, errCorrupt
		}
		sc := exactPow10[tag-1]
		q := b[1:]
		for j := range v {
			k := int32(uint32(q[3*j])|uint32(q[3*j+1])<<8|uint32(q[3*j+2])<<16) << 8 >> 8
			v[j] = float32(float64(k) / sc)
		}
		return 1 + 3*len(v), nil
	case tag >= 16 && tag <= 23:
		if len(b) < 2 {
			return 0, errCorrupt
		}
		w := uint(b[1])
		if w < 1 || w > 24 {
			return 0, errCorrupt
		}
		n := 2 + (len(v)*int(w)+7)/8
		if n > len(b) {
			return 0, errCorrupt
		}
		sc := exactPow10[tag-16]
		off := int64(1) << (w - 1)
		mask := uint64(1)<<w - 1
		q := b[2:n]
		var acc uint64
		var nacc uint
		i := 0
		for j := range v {
			for nacc < w {
				acc |= uint64(q[i]) << nacc
				i++
				nacc += 8
			}
			k := int64(acc&mask) - off
			acc >>= w
			nacc -= w
			v[j] = float32(float64(k) / sc)
		}
		return n, nil
	}
	return 0, errCorrupt
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
			v := make([]float32, c.Param)
			n, err := decodeVector(v, b[p:])
			if err != nil {
				return nil, err
			}
			p += n
			r[i] = VecVal(v)
		}
	}
	if p != len(b) {
		return nil, errCorrupt
	}
	return r, nil
}

var errCorrupt = fmt.Errorf("corrupt row encoding")

// encodeCompact encodes the non-vector columns of r (row encoding, vector
// columns omitted) for the in-memory row cache of arena tables.
func encodeCompact(s *Schema, r Row) []byte {
	var buf []byte
	for i, c := range s.Columns {
		v := r[i]
		switch c.Type {
		case sqlparse.TypeBigint:
			buf = binary.LittleEndian.AppendUint64(buf, uint64(v.I))
		case sqlparse.TypeVarchar:
			buf = binary.AppendUvarint(buf, uint64(len(v.S)))
			buf = append(buf, v.S...)
		}
	}
	return buf
}

// decodeCompact rebuilds a row from encodeCompact output; the vector column
// takes vec (the arena region, not copied).
func decodeCompact(s *Schema, b []byte, vec []float32) (Row, error) {
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
			r[i] = VecVal(vec)
		}
	}
	if p != len(b) {
		return nil, errCorrupt
	}
	return r, nil
}

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

var exactPow10 = [...]float64{1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
	1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21, 1e22}

// fastDecimal parses [+-]digits[.digits] with at most 15 significant digits
// in total. Such a value is float64(mantissa) / 10^frac with both operands
// exact, so the single division is correctly rounded and the result is
// bit-identical to strconv.ParseFloat(s, 64) (which takes the same exact
// path). ok=false means "use strconv".
func fastDecimal(s string) (float64, bool) {
	i, neg := 0, false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	var m uint64
	digits, frac, dot := 0, 0, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			m = m*10 + uint64(c-'0')
			digits++
			if dot {
				frac++
			}
		case c == '.' && !dot:
			dot = true
		default:
			return 0, false
		}
	}
	if digits == 0 || digits > 15 {
		return 0, false
	}
	f := float64(m) / exactPow10[frac]
	if neg {
		f = -f
	}
	return f, true
}

func parseFloat32(s string) (float32, error) {
	if s == "" {
		return 0, newErr(ErrVectorValue, "invalid vector element ''")
	}
	if f, ok := fastDecimal(s); ok {
		return float32(f), nil // |f| < 1e15: always finite in float32
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
	if dim > 0 {
		// Common case: parse in place without splitting into substrings.
		out := make([]float32, 0, dim)
		for rest := inner; ; {
			e := rest
			j := strings.IndexByte(rest, ',')
			if j >= 0 {
				e, rest = rest[:j], rest[j+1:]
			}
			if len(out) == dim {
				break // too many elements: report via the generic path
			}
			f, err := parseFloat32(strings.TrimSpace(e))
			if err != nil {
				return nil, err
			}
			out = append(out, f)
			if j < 0 {
				if len(out) == dim {
					return out, nil
				}
				break
			}
		}
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
