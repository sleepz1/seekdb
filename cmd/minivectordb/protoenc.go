package main

import (
	"bufio"
	"math"
	"strconv"
	"unicode/utf8"

	"minivectordb/internal/engine"
)

// Direct encoder for successful responses. It produces exactly the bytes of
// the encoding/json path in format.go (writeProtoResult with HTML escaping
// off) without reflection or per-value allocations; protoenc_test.go checks
// the two against each other. It is used when the request id is a scalar
// taken verbatim from the request (the fastDecode path), so no compaction of
// the id is needed.

var protoBuf []byte

func fastWriteResult(w *bufio.Writer, id []byte, r *engine.Result) {
	protoBuf = appendProtoResult(protoBuf[:0], id, r)
	w.Write(protoBuf)
	if cap(protoBuf) > 1<<20 {
		protoBuf = nil // do not keep a large response buffer alive
	}
}

func appendProtoResult(b []byte, id []byte, r *engine.Result) []byte {
	b = append(b, `{"id":`...)
	b = append(b, id...)
	if !r.IsQuery {
		b = append(b, `,"ok":true,"affected_rows":`...)
		b = strconv.AppendInt(b, r.Affected, 10)
		return append(b, '}', '\n')
	}
	b = append(b, `,"ok":true,"columns":[`...)
	for i, c := range r.Columns {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, c)
	}
	b = append(b, `],"rows":[`...)
	for i, row := range r.Rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j := range row {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendJSONValue(b, &row[j])
		}
		b = append(b, ']')
	}
	return append(b, ']', '}', '\n')
}

func appendJSONValue(b []byte, v *engine.Value) []byte {
	switch v.K {
	case engine.KInt:
		return strconv.AppendInt(b, v.I, 10)
	case engine.KBool:
		if v.I != 0 {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case engine.KFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return append(b, "null"...)
		}
		return appendJSONFloat(b, v.F)
	case engine.KStr:
		return appendJSONString(b, v.S)
	case engine.KVec:
		b = append(b, '[')
		for i, f := range v.V {
			if i > 0 {
				b = append(b, ',')
			}
			x := float64(f)
			if math.IsNaN(x) || math.IsInf(x, 0) {
				x = 0
			}
			b = appendJSONFloat(b, x)
		}
		return append(b, ']')
	}
	return append(b, "null"...)
}

// appendJSONFloat formats a finite float64 the way encoding/json does:
// shortest representation, exponent form below 1e-6 and from 1e21, with a
// two-digit exponent reduced to one digit when it starts with 0.
func appendJSONFloat(b []byte, f float64) []byte {
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && (b[n-3] == '-' || b[n-3] == '+') && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

const jsonHex = "0123456789abcdef"

// appendJSONString quotes s the way encoding/json does with HTML escaping
// off: ", \ and control characters escaped, invalid UTF-8 replaced by
// \ufffd, U+2028 and U+2029 escaped.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '\\', '"':
				b = append(b, '\\', c)
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', jsonHex[c>>4], jsonHex[c&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', '2', '0', '2', jsonHex[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
