package main

import (
	"errors"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Request decoding without encoding/json (which, with the reflection it
// needs, is a large part of the executable). parseRequest has the
// semantics of json.Unmarshal into protoRequest's exported fields:
//
//   - the whole line must be one valid JSON value (else a syntax error, and
//     nothing is set); nesting deeper than 10000 is an error;
//   - top-level null sets nothing; any other non-object is a type error;
//   - keys match the field names exactly, else case-insensitively with the
//     folding of encoding/json; unknown keys are ignored; the last of
//     duplicate keys wins;
//   - id receives the raw bytes of its value (null included);
//   - a string field accepts a string (escapes decoded, invalid UTF-8 and
//     lone surrogates replaced by U+FFFD) or null (no change); any other
//     value is a type error, reported after the rest has been decoded.
//
// jsonreq_test.go compares it with json.Unmarshal (error or not, and the
// decoded fields) on generated and mutated inputs. Error texts differ.

const maxJSONDepth = 10000

var (
	errJSONSyntax = errors.New("invalid JSON")
	errJSONDepth  = errors.New("JSON nested too deeply")
	errJSONType   = errors.New("request is not a JSON object with string fields")
)

func jsonWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipJSONWS(b []byte, i int) int {
	for i < len(b) && jsonWS(b[i]) {
		i++
	}
	return i
}

// skipJSONValue returns the end of the JSON value that starts at b[i]
// (no leading whitespace), or an error if it is not valid.
func skipJSONValue(b []byte, i, depth int) (int, error) {
	if i >= len(b) {
		return 0, errJSONSyntax
	}
	switch c := b[i]; {
	case c == '{' || c == '[':
		if depth >= maxJSONDepth {
			return 0, errJSONDepth
		}
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		i = skipJSONWS(b, i+1)
		if i < len(b) && b[i] == closer {
			return i + 1, nil
		}
		for {
			var err error
			if c == '{' {
				if i, err = skipJSONString(b, i); err != nil {
					return 0, err
				}
				i = skipJSONWS(b, i)
				if i >= len(b) || b[i] != ':' {
					return 0, errJSONSyntax
				}
				i = skipJSONWS(b, i+1)
			}
			if i, err = skipJSONValue(b, i, depth+1); err != nil {
				return 0, err
			}
			i = skipJSONWS(b, i)
			if i >= len(b) {
				return 0, errJSONSyntax
			}
			if b[i] == closer {
				return i + 1, nil
			}
			if b[i] != ',' {
				return 0, errJSONSyntax
			}
			i = skipJSONWS(b, i+1)
		}
	case c == '"':
		return skipJSONString(b, i)
	case c == 't':
		return skipJSONLiteral(b, i, "true")
	case c == 'f':
		return skipJSONLiteral(b, i, "false")
	case c == 'n':
		return skipJSONLiteral(b, i, "null")
	case c == '-' || '0' <= c && c <= '9':
		return skipJSONNumber(b, i)
	}
	return 0, errJSONSyntax
}

func skipJSONLiteral(b []byte, i int, lit string) (int, error) {
	if len(b)-i < len(lit) || string(b[i:i+len(lit)]) != lit {
		return 0, errJSONSyntax
	}
	return i + len(lit), nil
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func skipJSONNumber(b []byte, i int) (int, error) {
	if b[i] == '-' {
		i++
	}
	if i >= len(b) || !isDigit(b[i]) {
		return 0, errJSONSyntax
	}
	if b[i] == '0' {
		i++
	} else {
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	}
	if i < len(b) && b[i] == '.' {
		i++
		if i >= len(b) || !isDigit(b[i]) {
			return 0, errJSONSyntax
		}
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= len(b) || !isDigit(b[i]) {
			return 0, errJSONSyntax
		}
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	}
	return i, nil
}

func isHex(c byte) bool { return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

// skipJSONString returns the end of the string starting at b[i] == '"'.
// Control characters must be escaped; invalid UTF-8 is accepted (as by
// encoding/json, which replaces it when decoding).
func skipJSONString(b []byte, i int) (int, error) {
	if i >= len(b) || b[i] != '"' {
		return 0, errJSONSyntax
	}
	for i++; i < len(b); i++ {
		switch c := b[i]; {
		case c == '"':
			return i + 1, nil
		case c < 0x20:
			return 0, errJSONSyntax
		case c == '\\':
			i++
			if i >= len(b) {
				return 0, errJSONSyntax
			}
			switch b[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if len(b)-i < 5 || !isHex(b[i+1]) || !isHex(b[i+2]) || !isHex(b[i+3]) || !isHex(b[i+4]) {
					return 0, errJSONSyntax
				}
				i += 4
			default:
				return 0, errJSONSyntax
			}
		}
	}
	return 0, errJSONSyntax
}

func hexVal(c byte) rune {
	switch {
	case isDigit(c):
		return rune(c - '0')
	case 'a' <= c && c <= 'f':
		return rune(c - 'a' + 10)
	}
	return rune(c - 'A' + 10)
}

// getu4 decodes \uXXXX at the start of s, or returns -1.
func getu4(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' || !isHex(s[2]) || !isHex(s[3]) || !isHex(s[4]) || !isHex(s[5]) {
		return -1
	}
	return hexVal(s[2])<<12 | hexVal(s[3])<<8 | hexVal(s[4])<<4 | hexVal(s[5])
}

// unquoteJSON decodes a valid JSON string token (quotes included) the way
// encoding/json does.
func unquoteJSON(tok []byte) string {
	s := tok[1 : len(tok)-1]
	plain := true
	for _, c := range s {
		if c == '\\' || c >= utf8.RuneSelf {
			plain = false
			break
		}
	}
	if plain {
		return string(s)
	}
	out := make([]byte, 0, len(s)+8)
	for r := 0; r < len(s); {
		c := s[r]
		switch {
		case c == '\\':
			switch s[r+1] {
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				rr := getu4(s[r:])
				r += 6
				if utf16.IsSurrogate(rr) {
					if dec := utf16.DecodeRune(rr, getu4(s[r:])); dec != unicode.ReplacementChar {
						r += 6
						out = utf8.AppendRune(out, dec)
						continue
					}
					rr = unicode.ReplacementChar
				}
				out = utf8.AppendRune(out, rr)
				continue
			default: // '"', '\\', '/'
				out = append(out, s[r+1])
			}
			r += 2
		case c < utf8.RuneSelf:
			out = append(out, c)
			r++
		default:
			rr, size := utf8.DecodeRune(s[r:])
			out = utf8.AppendRune(out, rr)
			r += size
		}
	}
	return string(out)
}

// foldName is encoding/json's case folding for field-name matching.
func foldName(in string) string {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		if c := in[i]; c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(in[i:])
		out = utf8.AppendRune(out, unicode.ToUpper(unicode.SimpleFold(r)))
		i += n
	}
	return string(out)
}

func requestField(key string) int {
	switch key {
	case "id":
		return 0
	case "op":
		return 1
	case "sql":
		return 2
	case "search_mode":
		return 3
	}
	switch foldName(key) {
	case "ID":
		return 0
	case "OP":
		return 1
	case "SQL":
		return 2
	case "SEARCH_MODE":
		return 3
	}
	return -1
}

func parseRequest(b []byte, req *protoRequest) error {
	start := skipJSONWS(b, 0)
	end, err := skipJSONValue(b, start, 0)
	if err != nil {
		return err
	}
	if skipJSONWS(b, end) != len(b) {
		return errJSONSyntax
	}
	switch b[start] {
	case 'n':
		return nil
	case '{':
	default:
		return errJSONType
	}
	var typeErr error
	i := skipJSONWS(b, start+1)
	if b[i] == '}' {
		return nil
	}
	for {
		kend, _ := skipJSONString(b, i)
		key := unquoteJSON(b[i:kend])
		i = skipJSONWS(b, kend)
		i = skipJSONWS(b, i+1) // ':'
		vend, _ := skipJSONValue(b, i, 1)
		val := b[i:vend]
		switch f := requestField(key); f {
		case 0:
			req.ID = append(req.ID[:0:0], val...)
		case 1, 2, 3:
			switch val[0] {
			case '"':
				s := unquoteJSON(val)
				switch f {
				case 1:
					req.Op = s
				case 2:
					req.SQL = s
				case 3:
					req.SearchMode = s
				}
			case 'n':
			default:
				if typeErr == nil {
					typeErr = errJSONType
				}
			}
		}
		i = skipJSONWS(b, vend)
		if b[i] == '}' {
			return typeErr
		}
		i = skipJSONWS(b, i+1) // ','
	}
}

// validJSONScalar reports whether raw is exactly one valid JSON scalar.
func validJSONScalar(raw []byte) bool {
	if len(raw) == 0 || raw[0] == '{' || raw[0] == '[' {
		return false
	}
	end, err := skipJSONValue(raw, 0, 0)
	return err == nil && end == len(raw)
}

// appendCompactJSON appends a valid JSON value without insignificant
// whitespace (json.Compact; HTML escaping off).
func appendCompactJSON(dst, src []byte) []byte {
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			dst = append(dst, c)
			if c == '\\' {
				i++
				dst = append(dst, src[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if jsonWS(c) {
			continue
		}
		if c == '"' {
			inStr = true
		}
		dst = append(dst, c)
	}
	return dst
}
