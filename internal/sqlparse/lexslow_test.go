package sqlparse

import "strings"

func lexStringSlow(src string, i int) (string, int, error) {
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
