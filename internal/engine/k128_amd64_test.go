//go:build amd64

package engine

import (
	"math/rand"
	"testing"
)

// TestBlock128x8Agrees compares the eight-row 128-dimension kernels (and the
// dispatch through dotQ8Block, which hands the remainder rows to the general
// kernel) with the portable reference on random and extreme codes.
func TestBlock128x8Agrees(t *testing.T) {
	if !hasAVX2 {
		t.Skip("no AVX2")
	}
	rng := rand.New(rand.NewSource(12))
	const dim = 128
	for it := 0; it < 300; it++ {
		rows := 1 + rng.Intn(70)
		c := make([]int8, rows*dim)
		e := make([]int8, dim)
		for i := range c {
			if it%3 == 0 {
				c[i] = int8([]int{0, 127, -128, -1}[rng.Intn(4)])
			} else {
				c[i] = int8(rng.Intn(256))
			}
		}
		var sum int32
		for i := range e {
			if it%2 == 0 {
				e[i] = int8(63 - 126*rng.Intn(2))
			} else {
				e[i] = int8(rng.Intn(127) - 63)
			}
			sum += int32(e[i])
		}
		qq := &q8query{e: e, bias: 128 * sum}
		want := make([]int32, rows)
		for r := range want {
			want[r] = dotQ8Go(c[r*dim:(r+1)*dim], qq)
		}
		check := func(name string, got []int32) {
			for r := range got {
				if got[r] != want[r] {
					t.Fatalf("%s it=%d rows=%d row=%d: %d != %d", name, it, rows, r, got[r], want[r])
				}
			}
		}
		got := make([]int32, rows)
		dotQ8Block(c, qq, got)
		check("dispatch", got)
		n := rows &^ 7
		got = make([]int32, n)
		dotU8Block128x8(c, e, qq.bias, got)
		check("avx2", got)
		if hasVNNI {
			got = make([]int32, n)
			dotU8Block128x8VNNI(c, e, qq.bias, got)
			check("vnni", got)
		}
	}
}
