package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// parseDecimalListRef is parseDecimalList without the eight-byte fast path
// (the version admitted before round 15), kept to check that the fast path
// changes nothing.
func parseDecimalListRef(s string, out []float32) bool {
	i, n := 0, 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		neg := false
		if i < len(s) && (s[i] == '-' || s[i] == '+') {
			neg = s[i] == '-'
			i++
		}
		var m uint64
		digits, frac, dot := 0, 0, false
		for ; i < len(s); i++ {
			c := s[i]
			if c-'0' <= 9 {
				m = m*10 + uint64(c-'0')
				digits++
				if dot {
					frac++
				}
			} else if c == '.' && !dot {
				dot = true
			} else {
				break
			}
		}
		if digits == 0 || digits > 15 || n == len(out) {
			return false
		}
		f := float64(m) / exactPow10[frac]
		if neg {
			f = -f
		}
		out[n] = float32(f)
		n++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i == len(s) {
			return n == len(out)
		}
		if s[i] != ',' {
			return false
		}
		i++
	}
}

// TestParseDecimalListFastPath compares the parser with the reference on
// the benchmark's shape (every value of D.DDDDDD around the extremes and at
// random) and on near-misses: other digit counts, a second dot, signs,
// blanks, non-digit bytes in each position, truncated input.
func TestParseDecimalListFastPath(t *testing.T) {
	check := func(s string, dim int) {
		t.Helper()
		a := make([]float32, dim)
		b := make([]float32, dim)
		oka, okb := parseDecimalList(s, a), parseDecimalListRef(s, b)
		if oka != okb {
			t.Fatalf("%q: ok %v, reference %v", s, oka, okb)
		}
		if !oka {
			return
		}
		for i := range a {
			if math.Float32bits(a[i]) != math.Float32bits(b[i]) {
				t.Fatalf("%q: element %d = %v, reference %v", s, i, a[i], b[i])
			}
		}
	}
	for _, v := range []string{"0.000000", "9.999999", "1.000000", "0.000001", "5.500000", "0.999999", "9.000000"} {
		for _, sign := range []string{"", "-", "+"} {
			check(sign+v, 1)
			check(sign+v+","+sign+v, 2)
			check(" "+sign+v+" , "+v+" ", 2)
		}
	}
	rng := rand.New(rand.NewSource(15))
	for it := 0; it < 200000; it++ {
		d := rng.Intn(10)
		f := rng.Intn(1000000)
		check(fmt.Sprintf("%d.%06d", d, f), 1)
	}
	// near-misses
	base := []byte("0.352334")
	alphabet := []byte("0123456789.-+, /:eE\x00\xff")
	for pos := 0; pos < len(base); pos++ {
		for _, c := range alphabet {
			b := append([]byte(nil), base...)
			b[pos] = c
			check(string(b), 1)
			check(string(b)+",0.5", 2)
			check("1.5,"+string(b), 2)
		}
	}
	for _, s := range []string{"0.3523341", "0.35233", "10.352334", "0.352334.", "0.352334.5", "0.35233a", ".3523345",
		"0.352334,", ",0.352334", "0.352334 0.1", "00.352334", "0.352334e1", "-0.352334", "0.3523349999999999"} {
		check(s, 1)
		check(s+",1", 2)
		check("1,"+s, 2)
	}
	// a benchmark-like vector of 128 values
	var parts []string
	for i := 0; i < 128; i++ {
		parts = append(parts, fmt.Sprintf("%.6f", rng.Float64()*2-1))
	}
	check(strings.Join(parts, ","), 128)
	check(strings.Join(parts, ", "), 128)
}

func BenchmarkParseDecimalList(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var parts []string
	for i := 0; i < 128; i++ {
		parts = append(parts, fmt.Sprintf("%.6f", rng.Float64()*2-1))
	}
	s := strings.Join(parts, ",")
	out := make([]float32, 128)
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			parseDecimalList(s, out)
		}
	})
	b.Run("ref", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			parseDecimalListRef(s, out)
		}
	})
}
