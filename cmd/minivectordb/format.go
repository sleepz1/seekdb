package main

import (
	"bufio"
	"encoding/json"
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
	w.WriteString(`{"error":{"name":`)
	writeJSONString(w, e.Name())
	w.WriteString(`,"code":`)
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

// tsvFormatter is the default stdout framing. It is byte-compatible with the
// interim framing the team's earlier submission used, which the official
// driver ran to completion:
//
//	SELECT:  column names joined by TAB, one line per row (TAB-separated),
//	         then "OK <row count>"
//	writes:  "OK <affected rows>" (DDL / transaction control: "OK 0")
//	errors:  "ERROR <code> <message>", code = semantic name (duplicate_key, ...)
//
// Strings escape backslash, TAB, LF and CR; numbers and vectors use Go
// encoding/json rendering (5.0 -> 5, vectors -> [3,4]).
type tsvFormatter struct{}

func (tsvFormatter) writeResult(w *bufio.Writer, r *engine.Result) {
	if !r.IsQuery {
		w.WriteString("OK ")
		w.WriteString(strconv.FormatInt(r.Affected, 10))
		w.WriteByte('\n')
		return
	}
	w.WriteString(strings.Join(r.Columns, "\t"))
	w.WriteByte('\n')
	for _, row := range r.Rows {
		for j, v := range row {
			if j > 0 {
				w.WriteByte('\t')
			}
			w.WriteString(tsvCell(v))
		}
		w.WriteByte('\n')
	}
	w.WriteString("OK ")
	w.WriteString(strconv.Itoa(len(r.Rows)))
	w.WriteByte('\n')
}

func (tsvFormatter) writeError(w *bufio.Writer, e *engine.SQLError) {
	w.WriteString("ERROR ")
	w.WriteString(e.Name())
	w.WriteByte(' ')
	w.WriteString(escapeTSV(e.Msg))
	w.WriteByte('\n')
}

func tsvCell(v engine.Value) string {
	switch v.K {
	case engine.KStr:
		return escapeTSV(v.S)
	case engine.KInt:
		return strconv.FormatInt(v.I, 10)
	case engine.KBool:
		if v.I != 0 {
			return "true"
		}
		return "false"
	case engine.KFloat:
		b, err := json.Marshal(v.F)
		if err != nil {
			return "null"
		}
		return string(b)
	case engine.KVec:
		b, err := json.Marshal(v.V)
		if err != nil {
			return "null"
		}
		return string(b)
	}
	return "null"
}

var tsvEscaper = strings.NewReplacer("\\", `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func escapeTSV(s string) string { return tsvEscaper.Replace(s) }

// Official JSONL protocol writers (STDIO_PROTOCOL.md). Each call emits one
// JSON object and a trailing newline; the caller must flush immediately.

func writeProtoMsg(w *bufio.Writer, msg any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		// A well-formed protocol struct should always encode. If it does
		// not, writing a fallback would risk a second broken line; leave
		// the error to the next flush / process failure path.
		return
	}
}

func writeProtoShutdown(w *bufio.Writer, id json.RawMessage) {
	writeProtoMsg(w, struct {
		ID json.RawMessage `json:"id"`
		OK bool            `json:"ok"`
	}{ID: id, OK: true})
}

func writeProtoError(w *bufio.Writer, id json.RawMessage, code, message string) {
	if code == "" {
		code = "internal_error"
	}
	writeProtoMsg(w, struct {
		ID    json.RawMessage `json:"id"`
		OK    bool            `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		ID: id,
		OK: false,
		Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: code, Message: message},
	})
}

func writeProtoResult(w *bufio.Writer, id json.RawMessage, r *engine.Result) {
	if r.IsQuery {
		cols := r.Columns
		if cols == nil {
			cols = []string{}
		}
		rows := make([][]any, len(r.Rows))
		for i, row := range r.Rows {
			rows[i] = rowToJSON(row)
		}
		if rows == nil {
			rows = [][]any{}
		}
		writeProtoMsg(w, struct {
			ID      json.RawMessage `json:"id"`
			OK      bool            `json:"ok"`
			Columns []string        `json:"columns"`
			Rows    [][]any         `json:"rows"`
		}{ID: id, OK: true, Columns: cols, Rows: rows})
		return
	}
	writeProtoMsg(w, struct {
		ID           json.RawMessage `json:"id"`
		OK           bool            `json:"ok"`
		AffectedRows int64           `json:"affected_rows"`
	}{ID: id, OK: true, AffectedRows: r.Affected})
}

func rowToJSON(row []engine.Value) []any {
	out := make([]any, len(row))
	for i, v := range row {
		out[i] = valueToJSON(v)
	}
	return out
}

func valueToJSON(v engine.Value) any {
	switch v.K {
	case engine.KInt:
		return v.I
	case engine.KBool:
		return v.I != 0
	case engine.KFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return nil
		}
		return v.F
	case engine.KStr:
		return v.S
	case engine.KVec:
		arr := make([]float64, len(v.V))
		for i, f := range v.V {
			x := float64(f)
			if math.IsNaN(x) || math.IsInf(x, 0) {
				// Protocol forbids NaN/Infinity; a non-finite component
				// should not have reached here, but keep the array finite.
				arr[i] = 0
			} else {
				arr[i] = x
			}
		}
		return arr
	default:
		return nil
	}
}
