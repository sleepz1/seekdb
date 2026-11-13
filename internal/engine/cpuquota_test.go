package engine

import "testing"

func TestQuotaCPUs(t *testing.T) {
	for _, c := range []struct {
		q, p string
		want int
	}{
		{"max", "100000", 0},
		{"-1", "100000", 0},
		{"200000", "100000", 2},
		{"150000", "100000", 2},
		{"50000", "100000", 1},
		{"100000", "0", 0},
		{"x", "100000", 0},
	} {
		if got := quotaCPUs(c.q, c.p); got != c.want {
			t.Errorf("quotaCPUs(%q, %q) = %d, want %d", c.q, c.p, got, c.want)
		}
	}
}
