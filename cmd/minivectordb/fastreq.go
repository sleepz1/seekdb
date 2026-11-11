package main

import (
	"encoding/binary"
	"unicode/utf8"
)

// decodeRequest decodes one protocol request. The common shape (a flat
// object whose keys are exactly id/op/sql/search_mode, string values without
// escapes, a scalar id) is scanned directly; anything else, including every
// malformed line, goes through parseRequest (json.Unmarshal semantics, see
// jsonreq.go).
func decodeRequest(line []byte, req *protoRequest) error {
	if fastDecode(line, req) {
		req.fast = true
		return nil
	}
	*req = protoRequest{}
	return parseRequest(line, req)
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func fastDecode(b []byte, req *protoRequest) bool {
	i := 0
	skip := func() {
		for i < len(b) && isWS(b[i]) {
			i++
		}
	}
	// str scans a JSON string without escapes starting at b[i] == '"'.
	str := func() (string, bool) {
		if i >= len(b) || b[i] != '"' {
			return "", false
		}
		j := i + 1
		// eight bytes at a time while none of them is '"', '\\' or a
		// control character
		for j+8 <= len(b) {
			x := binary.LittleEndian.Uint64(b[j:])
			ctl := (x - 0x2020202020202020) &^ x
			q := x ^ 0x2222222222222222
			bs := x ^ 0x5c5c5c5c5c5c5c5c
			if (ctl|(q-0x0101010101010101)&^q|(bs-0x0101010101010101)&^bs)&0x8080808080808080 != 0 {
				break
			}
			j += 8
		}
		for j < len(b) {
			c := b[j]
			if c == '"' {
				break
			}
			if c == '\\' || c < 0x20 {
				return "", false
			}
			j++
		}
		if j >= len(b) {
			return "", false
		}
		s := b[i+1 : j]
		if !utf8.Valid(s) {
			return "", false // json.Unmarshal would substitute U+FFFD
		}
		i = j + 1
		return string(s), true
	}
	var seen [4]bool
	skip()
	if i >= len(b) || b[i] != '{' {
		return false
	}
	i++
	skip()
	if i < len(b) && b[i] == '}' {
		i++
	} else {
		for {
			skip()
			key, ok := str()
			if !ok {
				return false
			}
			skip()
			if i >= len(b) || b[i] != ':' {
				return false
			}
			i++
			skip()
			k := -1
			switch key {
			case "id":
				k = 0
			case "op":
				k = 1
			case "sql":
				k = 2
			case "search_mode":
				k = 3
			}
			if k < 0 || seen[k] {
				return false // unknown/duplicate key: let encoding/json decide
			}
			seen[k] = true
			if k == 0 {
				start := i
				if i < len(b) && b[i] == '"' {
					if _, ok := str(); !ok {
						return false
					}
				} else {
					for i < len(b) && b[i] != ',' && b[i] != '}' && !isWS(b[i]) {
						i++
					}
				}
				raw := b[start:i]
				if !validJSONScalar(raw) {
					return false
				}
				req.ID = append([]byte(nil), raw...)
			} else {
				v, ok := str()
				if !ok {
					return false
				}
				switch k {
				case 1:
					req.Op = v
				case 2:
					req.SQL = v
				case 3:
					req.SearchMode = v
				}
			}
			skip()
			if i >= len(b) {
				return false
			}
			if b[i] == ',' {
				i++
				continue
			}
			if b[i] == '}' {
				i++
				break
			}
			return false
		}
	}
	skip()
	return i == len(b)
}
