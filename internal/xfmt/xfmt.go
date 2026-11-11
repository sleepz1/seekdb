// Package xfmt is the small subset of fmt that the server needs: %s, %d,
// %q, %v, %w and %% with strings, integers, booleans, floats, errors and
// Stringers. Linking fmt (and the reflection it pulls in) costs about half a
// megabyte of executable, which every cold start reads from disk and keeps
// in the page cache. Tests check every call against fmt.Sprintf (see
// Check).
package xfmt

import (
	"strconv"
)

// Check, when set (by tests), is called with every formatted result.
var Check func(format string, a []any, got string)

type stringer interface{ String() string }

// Sprintf formats like fmt.Sprintf for the supported verbs and types.
func Sprintf(format string, a ...any) string {
	b := make([]byte, 0, len(format)+16*len(a))
	n := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b = append(b, c)
			continue
		}
		if i+1 == len(format) {
			b = append(b, "%!(NOVERB)"...)
			continue
		}
		i++
		verb := format[i]
		if verb == '%' {
			b = append(b, '%')
			continue
		}
		if n >= len(a) {
			b = append(b, "%!"...)
			b = append(b, verb)
			b = append(b, "(MISSING)"...)
			continue
		}
		b = appendArg(b, verb, a[n])
		n++
	}
	if n < len(a) {
		b = append(b, "%!(EXTRA "...)
		for j, x := range a[n:] {
			if j > 0 {
				b = append(b, ", "...)
			}
			b = append(b, typeName(x)...)
			b = append(b, '=')
			b = appendArg(b, 'v', x)
		}
		b = append(b, ')')
	}
	s := string(b)
	if Check != nil {
		Check(format, a, s)
	}
	return s
}

func typeName(x any) string {
	switch x.(type) {
	case string:
		return "string"
	case int:
		return "int"
	case int64:
		return "int64"
	case error:
		return "error"
	}
	return "?"
}

func appendArg(b []byte, verb byte, x any) []byte {
	if x == nil {
		return append(b, "%!"+string(verb)+"(<nil>)"...)
	}
	switch v := x.(type) {
	case error:
		if verb == 'q' {
			return strconv.AppendQuote(b, v.Error())
		}
		return append(b, v.Error()...)
	case stringer:
		if verb == 'q' {
			return strconv.AppendQuote(b, v.String())
		}
		return append(b, v.String()...)
	case string:
		if verb == 'q' {
			return strconv.AppendQuote(b, v)
		}
		return append(b, v...)
	case []byte:
		if verb == 'q' {
			return strconv.AppendQuote(b, string(v))
		}
		return append(b, v...)
	case bool:
		return strconv.AppendBool(b, v)
	case int:
		return appendInt(b, verb, int64(v))
	case int8:
		return appendInt(b, verb, int64(v))
	case int16:
		return appendInt(b, verb, int64(v))
	case int32:
		return appendInt(b, verb, int64(v))
	case int64:
		return appendInt(b, verb, v)
	case uint:
		return appendUint(b, verb, uint64(v))
	case uint8:
		return appendUint(b, verb, uint64(v))
	case uint16:
		return appendUint(b, verb, uint64(v))
	case uint32:
		return appendUint(b, verb, uint64(v))
	case uint64:
		return appendUint(b, verb, v)
	case float64:
		return strconv.AppendFloat(b, v, 'g', -1, 64)
	case float32:
		return strconv.AppendFloat(b, float64(v), 'g', -1, 32)
	}
	return append(b, "%!"+string(verb)+"(?)"...)
}

func appendInt(b []byte, verb byte, v int64) []byte {
	if verb == 'q' {
		return strconv.AppendQuoteRune(b, rune(v))
	}
	return strconv.AppendInt(b, v, 10)
}

func appendUint(b []byte, verb byte, v uint64) []byte {
	if verb == 'q' {
		return strconv.AppendQuoteRune(b, rune(v))
	}
	return strconv.AppendUint(b, v, 10)
}

// wrapped is the error Errorf returns; it unwraps to its %w argument.
type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }

type plain struct{ msg string }

func (p *plain) Error() string { return p.msg }

// Errorf is fmt.Errorf for the supported verbs: with one %w argument the
// result unwraps to it.
func Errorf(format string, a ...any) error {
	msg := Sprintf(format, a...)
	n := 0
	for i := 0; i+1 < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if format[i] == '%' {
			continue
		}
		if format[i] == 'w' && n < len(a) {
			if e, ok := a[n].(error); ok {
				return &wrapped{msg, e}
			}
		}
		n++
	}
	return &plain{msg}
}
