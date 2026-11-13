package engine

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// parseVectorTextRef is the parser before the single-pass fast path.
func parseVectorTextRef(s string, dim int) ([]float32, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, newErr(ErrVectorValue, "invalid vector literal '%s'", s)
	}
	inner := strings.TrimSpace(t[1 : len(t)-1])
	if inner == "" {
		return parseVectorElems(nil, dim)
	}
	if dim > 0 {
		// Common case: parse in place without splitting into substrings.
		out := make([]float32, 0, dim)
		for rest := inner; ; {
			e := rest
			j := strings.IndexByte(rest, ',')
			if j >= 0 {
				e, rest = rest[:j], rest[j+1:]
			}
			if len(out) == dim {
				break // too many elements: report via the generic path
			}
			f, err := parseFloat32(strings.TrimSpace(e))
			if err != nil {
				return nil, err
			}
			out = append(out, f)
			if j < 0 {
				if len(out) == dim {
					return out, nil
				}
				break
			}
		}
	}
	return parseVectorElems(strings.Split(inner, ","), dim)
}

// The fast path must give the same vectors and the same errors.
func TestParseVectorTextMatchesRef(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	elems := []string{"0.123456", "-0.5", "+1", "1.", ".5", "-.25", "0", "-0", "-0.000000", "123456789012345", "1234567890123456",
		"1e3", "1E-2", "nan", "inf", "0x1p3", "1_0", "", " ", "\u00a00.5", "\v1", "..5", "1..", "--1", "+-1", "3.4e39", "1e-50",
		"0.0000001", "99999999999.9999", "007", "8388607.5", "-3.14159265358979"}
	seps := []string{",", ", ", " ,", "\t,\n", ",,", " , "}
	for it := 0; it < 200000; it++ {
		dim := rng.Intn(6)
		if rng.Intn(3) == 0 {
			dim = 128
		}
		n := dim
		if rng.Intn(10) == 0 {
			n += rng.Intn(3) - 1
		}
		var sb strings.Builder
		sb.WriteString([]string{"[", " [", "[ ", "\n["}[rng.Intn(4)])
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(seps[rng.Intn(len(seps))])
			}
			if rng.Intn(4) == 0 {
				sb.WriteString(elems[rng.Intn(len(elems))])
			} else {
				f := (rng.Float64()*2 - 1) * math.Pow(10, float64(rng.Intn(5)))
				sb.WriteString(strings.TrimRight(strings.TrimRight(formatFixed(f, rng.Intn(8)), "0"), "."))
			}
		}
		sb.WriteString([]string{"]", " ]", "] ", "]x"}[rng.Intn(4)])
		in := strings.ReplaceAll(strings.ReplaceAll(sb.String(), "\\u00a0", "\u00a0"), "\\v", "\v")
		got, errG := parseVectorText(in, dim)
		want, errW := parseVectorTextRef(in, dim)
		if (errG == nil) != (errW == nil) || errG != nil && errG.Error() != errW.Error() {
			t.Fatalf("%q dim %d: error %v, ref %v", in, dim, errG, errW)
		}
		if len(got) != len(want) {
			t.Fatalf("%q: len %d, ref %d", in, len(got), len(want))
		}
		for i := range got {
			if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
				t.Fatalf("%q elem %d: %v, ref %v", in, i, got[i], want[i])
			}
		}
	}
}

func formatFixed(f float64, prec int) string {
	s := make([]byte, 0, 24)
	return string(appendFixed(s, f, prec))
}

func appendFixed(b []byte, f float64, prec int) []byte {
	return strconvAppendFloat(b, f, 'f', prec, 64)
}

var strconvAppendFloat = strconv.AppendFloat
