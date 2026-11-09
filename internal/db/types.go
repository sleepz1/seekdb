package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

func parseVector(p string, dim int) ([]float32, error) {
	var values []json.RawMessage
	if err := json.Unmarshal([]byte(p), &values); err != nil || len(values) != dim {
		return nil, fmt.Errorf("vector dimension or syntax mismatch (expected %d finite float32 values)", dim)
	}
	v := make([]float32, dim)
	for i, raw := range values {
		n, err := strconv.ParseFloat(string(raw), 32)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("vector requires finite float32 values; invalid value %s", raw)
		}
		v[i] = float32(n)
	}
	return v, nil
}

func parseValue(c Column, p string) (any, error) {
	switch c.Type {
	case "BIGINT", "INT", "INTEGER":
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s requires an integer in int64 range", c.Type)
		}
		return n, nil
	case "TEXT", "VARCHAR":
		if len(p) < 2 || p[0] != '\'' || p[len(p)-1] != '\'' {
			return nil, fmt.Errorf("%s requires SQL quotes", c.Type)
		}
		inner := p[1 : len(p)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\'' {
				if i+1 >= len(inner) || inner[i+1] != '\'' {
					return nil, fmt.Errorf("invalid SQL string quote")
				}
				i++
			}
		}
		value := strings.ReplaceAll(inner, "''", "'")
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("invalid UTF-8 string")
		}
		if c.CharLen > 0 && utf8.RuneCountInString(value) > c.CharLen {
			return nil, fmt.Errorf("VARCHAR(%d) exceeded: %d Unicode characters", c.CharLen, utf8.RuneCountInString(value))
		}
		return value, nil
	case "VECTOR":
		return parseVector(p, c.Dim)
	case "FLOAT", "DOUBLE":
		n, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("invalid finite number")
		}
		return n, nil
	default:
		return nil, fmt.Errorf("unsupported column type %s", c.Type)
	}
}

type predicate struct {
	index int
	value any
}

var equalityRE = regexp.MustCompile(`(?is)^\s*(\w+)\s*=\s*('(?:[^']|'')*'|[^\s']+)\s*(.*)$`)
var andRE = regexp.MustCompile(`(?is)^AND\s+(.+)$`)

func parseWhere(t *Table, clause string) ([]predicate, error) {
	var out []predicate
	for clause != "" {
		m := equalityRE.FindStringSubmatch(clause)
		if m == nil {
			return nil, fmt.Errorf("unsupported WHERE; expected column = literal AND ...")
		}
		idx := -1
		for i, c := range t.Columns {
			if c.Name == strings.ToLower(m[1]) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("unknown WHERE column %s", m[1])
		}
		if t.Columns[idx].Type == "VECTOR" {
			return nil, fmt.Errorf("WHERE requires ordinary column")
		}
		v, err := parseValue(t.Columns[idx], m[2])
		if err != nil {
			return nil, err
		}
		out = append(out, predicate{idx, v})
		clause = strings.TrimSpace(m[3])
		if clause != "" {
			a := andRE.FindStringSubmatch(clause)
			if a == nil {
				return nil, fmt.Errorf("unsupported WHERE operator")
			}
			clause = a[1]
		}
	}
	return out, nil
}

// annCandidates is a deterministic approximate scan, not a vector index. Sampling
// after predicates preserves filter-before-TopK semantics but does not guarantee recall.
func annCandidates(rows [][]any) [][]any {
	out := make([][]any, 0, (len(rows)+1)/2)
	for i := 0; i < len(rows); i += 2 {
		out = append(out, rows[i])
	}
	return out
}

// Decode numbers without float64 conversion, then validate and restore column types.
func decodeState(b []byte) (State, error) {
	var state State
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing snapshot data")
	}
	if state == nil {
		return nil, fmt.Errorf("invalid null snapshot")
	}
	for _, t := range state {
		if t == nil {
			return nil, fmt.Errorf("invalid null table")
		}
		for _, row := range t.Rows {
			if len(row) != len(t.Columns) {
				return nil, fmt.Errorf("snapshot column count mismatch")
			}
			for i, c := range t.Columns {
				var literal string
				if c.Type == "TEXT" || c.Type == "VARCHAR" {
					v, ok := row[i].(string)
					if !ok {
						return nil, fmt.Errorf("invalid snapshot string")
					}
					literal = "'" + strings.ReplaceAll(v, "'", "''") + "'"
				} else {
					raw, err := json.Marshal(row[i])
					if err != nil {
						return nil, err
					}
					literal = string(raw)
				}
				v, err := parseValue(c, literal)
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
		}
	}
	return state, nil
}
