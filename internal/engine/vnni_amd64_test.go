//go:build amd64

package engine

import (
	"math/rand"
	"testing"
)

// TestVNNIKernelsAgree compares the VPDPBUSD kernels with the AVX2 ones on
// random and extreme codes, for dimensions with and without a 16-byte tail.
func TestVNNIKernelsAgree(t *testing.T) {
	if !hasVNNI || !hasAVX2 {
		t.Skip("no AVX512-VNNI on this CPU")
	}
	rng := rand.New(rand.NewSource(11))
	for _, dim := range []int{16, 32, 48, 64, 80, 128, 144, 256, 512} {
		for it := 0; it < 100; it++ {
			rows := 1 + rng.Intn(23)
			c := make([]int8, rows*dim)
			e := make([]int8, dim)
			for i := range c {
				if it%3 == 0 {
					c[i] = int8([]int{0, 127, -128, -1}[rng.Intn(4)])
				} else {
					c[i] = int8(rng.Intn(256))
				}
			}
			for i := range e {
				if it%2 == 0 {
					e[i] = int8(63 - 126*rng.Intn(2))
				} else {
					e[i] = int8(rng.Intn(127) - 63)
				}
			}
			bias := int32(rng.Intn(1 << 20))
			want := make([]int32, rows)
			got := make([]int32, rows)
			dotU8Block(c, e, bias, want)
			dotU8BlockVNNI(c, e, bias, got)
			for r := range want {
				if got[r] != want[r] {
					t.Fatalf("block dim=%d it=%d row=%d: %d != %d", dim, it, r, got[r], want[r])
				}
			}
			idx := make([]int32, 1+rng.Intn(2*rows))
			for i := range idx {
				idx[i] = int32(rng.Intn(rows))
			}
			want = make([]int32, len(idx))
			got = make([]int32, len(idx))
			dotU8Rows(c, idx, e, bias, want)
			dotU8RowsVNNI(c, idx, e, bias, got)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("rows dim=%d it=%d i=%d: %d != %d", dim, it, i, got[i], want[i])
				}
			}
		}
	}
}
