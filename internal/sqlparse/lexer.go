// Package sqlparse implements the lexer and parser for the MiniVectorDB SQL subset.
package sqlparse

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tQuotedIdent
	tNumber
	tString
	tSymbol
)

type token struct {
	kind tokKind
	text string // identifier (original case), number text, string value, or symbol
	pos  int
}

func (t token) upper() string { return strings.ToUpper(t.text) }

// SyntaxError is returned for malformed input.
type SyntaxError struct{ Msg string }

func (e *SyntaxError) Error() string { return e.Msg }

func syntaxErr(format string, a ...any) error {
	return &SyntaxError{Msg: fmt.Sprintf(format, a...)}
}

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, syntaxErr("unterminated comment")
			}
			i += end + 4
		case c == '\'' || c == '"':
			s, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tString, text: s, pos: i})
			i = next
		case c == '`':
			j := i + 1
			var sb strings.Builder
			for {
				if j >= n {
					return nil, syntaxErr("unterminated quoted identifier")
				}
				if src[j] == '`' {
					if j+1 < n && src[j+1] == '`' {
						sb.WriteByte('`')
						j += 2
						continue
					}
					break
				}
				sb.WriteByte(src[j])
				j++
			}
			toks = append(toks, token{kind: tQuotedIdent, text: sb.String(), pos: i})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < n && src[j] == '.' {
				j++
				for j < n && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < n && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < n && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < n && src[k] >= '0' && src[k] <= '9' {
					for k < n && src[k] >= '0' && src[k] <= '9' {
						k++
					}
					j = k
				}
			}
			if j < n && isIdentByte(src[j]) {
				return nil, syntaxErr("syntax error near '%s'", near(src, i))
			}
			toks = append(toks, token{kind: tNumber, text: src[i:j], pos: i})
			i = j
		case isIdentStart(src, i):
			j := i
			for j < n {
				r, size := utf8.DecodeRuneInString(src[j:])
				if r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) {
					j += size
					continue
				}
				break
			}
			toks = append(toks, token{kind: tIdent, text: src[i:j], pos: i})
			i = j
		default:
			two := ""
			if i+1 < n {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "!=", "<>":
				toks = append(toks, token{kind: tSymbol, text: two, pos: i})
				i += 2
				continue
			}
			switch c {
			case '(', ')', ',', ';', '*', '=', '<', '>', '.', '[', ']', '-', '+':
				toks = append(toks, token{kind: tSymbol, text: string(c), pos: i})
				i++
			default:
				return nil, syntaxErr("syntax error near '%s'", near(src, i))
			}
		}
	}
	toks = append(toks, token{kind: tEOF, pos: n})
	return toks, nil
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func isIdentStart(src string, i int) bool {
	r, _ := utf8.DecodeRuneInString(src[i:])
	return r == '_' || unicode.IsLetter(r)
}

func near(src string, i int) string {
	end := i + 20
	if end > len(src) {
		end = len(src)
	}
	return src[i:end]
}

// lexString reads a MySQL style quoted string starting at src[i]. It supports
// doubled quotes and backslash escapes.
func lexString(src string, i int) (string, int, error) {
	q := src[i]
	j := i + 1
	var sb strings.Builder
	for {
		if j >= len(src) {
			return "", 0, syntaxErr("unterminated string literal")
		}
		c := src[j]
		if c == q {
			if j+1 < len(src) && src[j+1] == q {
				sb.WriteByte(q)
				j += 2
				continue
			}
			return sb.String(), j + 1, nil
		}
		if c == '\\' && j+1 < len(src) {
			e := src[j+1]
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '0':
				sb.WriteByte(0)
			case 'b':
				sb.WriteByte('\b')
			case 'Z':
				sb.WriteByte(0x1a)
			case '%', '_':
				sb.WriteByte('\\')
				sb.WriteByte(e)
			default:
				sb.WriteByte(e)
			}
			j += 2
			continue
		}
		sb.WriteByte(c)
		j++
	}
}
