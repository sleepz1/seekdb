package main

import (
	"bufio"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"minivectordb/internal/engine"
)

// formatter renders statement responses. Every response is exactly one line
// so a driver can read responses line by line.
type formatter interface {
	writeResult(w *bufio.Writer, r *engine.Result)
	writeError(w *bufio.Writer, e *engine.SQLError)
}

// jsonFormatter emits one JSON object per statement, using the same field
// names as the public .result assertions:
//
//	{"columns":["id","distance"],"rows":[[2,1.0],[3,2.0]]}
//	{"affected_rows":3}
//	{"error":{"code":1062,"sqlstate":"23000","message":"..."}}
type jsonFormatter struct{}

func (jsonFormatter) writeResult(w *bufio.Writer, r *engine.Result) {
	if !r.IsQuery {
		w.WriteString(`{"affected_rows":`)
		w.WriteString(strconv.FormatInt(r.Affected, 10))
		w.WriteString("}\n")
		return
	}
	w.WriteString(`{"columns":[`)
	for i, c := range r.Columns {
		if i > 0 {
			w.WriteByte(',')
		}
		writeJSONString(w, c)
	}
	w.WriteString(`],"rows":[`)
	for i, row := range r.Rows {
		if i > 0 {
			w.WriteByte(',')
		}
		w.WriteByte('[')
		for j, v := range row {
			if j > 0 {
				w.WriteByte(',')
			}
			writeJSONValue(w, v)
		}
		w.WriteByte(']')
	}
	w.WriteString("]}\n")
}

func (jsonFormatter) writeError(w *bufio.Writer, e *engine.SQLError) {
	w.WriteString(`{"error":{"code":`)
	w.WriteString(strconv.Itoa(e.Code()))
	w.WriteString(`,"sqlstate":`)
	writeJSONString(w, e.SQLState())
	w.WriteString(`,"message":`)
	writeJSONString(w, e.Msg)
	w.WriteString("}}\n")
}

func writeJSONValue(w *bufio.Writer, v engine.Value) {
	switch v.K {
	case engine.KInt, engine.KBool:
		w.WriteString(strconv.FormatInt(v.I, 10))
	case engine.KFloat:
		w.WriteString(formatFloat(v.F))
	case engine.KStr:
		writeJSONString(w, v.S)
	case engine.KVec:
		writeJSONString(w, engine.FormatVector(v.V))
	default:
		w.WriteString("null")
	}
}

// formatFloat prints the shortest round-trip form and always includes a
// decimal point or exponent so the value reads back as a float.
func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

const hexDigits = "0123456789abcdef"

func writeJSONString(w *bufio.Writer, s string) {
	w.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"':
				w.WriteString(`\"`)
			case c == '\\':
				w.WriteString(`\\`)
			case c == '\n':
				w.WriteString(`\n`)
			case c == '\r':
				w.WriteString(`\r`)
			case c == '\t':
				w.WriteString(`\t`)
			case c < 0x20 || c == 0x7f:
				w.WriteString(`\u00`)
				w.WriteByte(hexDigits[c>>4])
				w.WriteByte(hexDigits[c&0xf])
			default:
				w.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			w.WriteString(`�`)
		} else if r == ' ' || r == ' ' {
			w.WriteString(`\u202`)
			w.WriteByte(hexDigits[r&0xf])
		} else {
			w.WriteString(s[i : i+size])
		}
		i += size
	}
	w.WriteByte('"')
}

// tsvFormatter is a mysql --batch like rendering kept as an alternative:
// header line, one line per row, then "OK <n>"; errors as "ERROR <code> (<state>): <msg>".
type tsvFormatter struct{}

func (tsvFormatter) writeResult(w *bufio.Writer, r *engine.Result) {
	if !r.IsQuery {
		w.WriteString("OK ")
		w.WriteString(strconv.FormatInt(r.Affected, 10))
		w.WriteByte('\n')
		return
	}
	w.WriteString(strings.Join(escapeAll(r.Columns), "\t"))
	w.WriteByte('\n')
	for _, row := range r.Rows {
		for j, v := range row {
			if j > 0 {
				w.WriteByte('\t')
			}
			switch v.K {
			case engine.KInt, engine.KBool:
				w.WriteString(strconv.FormatInt(v.I, 10))
			case engine.KFloat:
				w.WriteString(formatFloat(v.F))
			case engine.KStr:
				w.WriteString(escapeTSV(v.S))
			case engine.KVec:
				w.WriteString(engine.FormatVector(v.V))
			default:
				w.WriteString("NULL")
			}
		}
		w.WriteByte('\n')
	}
	w.WriteString("OK ")
	w.WriteString(strconv.Itoa(len(r.Rows)))
	w.WriteByte('\n')
}

func (tsvFormatter) writeError(w *bufio.Writer, e *engine.SQLError) {
	w.WriteString("ERROR ")
	w.WriteString(strconv.Itoa(e.Code()))
	w.WriteString(" (")
	w.WriteString(e.SQLState())
	w.WriteString("): ")
	w.WriteString(escapeTSV(e.Msg))
	w.WriteByte('\n')
}

func escapeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = escapeTSV(s)
	}
	return out
}

var tsvEscaper = strings.NewReplacer("\\", `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`, "\x00", `\0`)

func escapeTSV(s string) string { return tsvEscaper.Replace(s) }
