package sqlparse

import "testing"

func TestLexStringFastPathMatchesSlowPath(t *testing.T) {
	for _, src := range []string{`'abc'`, `''`, `'it''s'`, `'a\nb'`, `'x\'y'`, `'[1,2,3]' rest`, `"dq"`, `"a""b"`, `'unterminated`, `'trail\`, `'a''`} {
		got, gn, gerr := lexString(src, 0)
		want, wn, werr := lexStringSlow(src, 0)
		if got != want || gn != wn || (gerr == nil) != (werr == nil) {
			t.Fatalf("%q: fast (%q,%d,%v) slow (%q,%d,%v)", src, got, gn, gerr, want, wn, werr)
		}
	}
}
