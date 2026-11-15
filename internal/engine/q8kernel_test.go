package engine

import (
	"math/rand"
	"testing"
)

// TestQ8KernelsMatchGo compares the SIMD dot kernels of this architecture
// with the portable reference on random and extreme codes.
func TestQ8KernelsMatchGo(t *testing.T) {
	if !hasQ8Kernel() {
		t.Skip("no int8 kernel on this CPU")
	}
	rng := rand.New(rand.NewSource(77))
	for _, dim := range []int{16, 32, 48, 64, 128, 144, 256, 512} {
		for it := 0; it < 200; it++ {
			rows := 1 + rng.Intn(37)
			c := make([]int8, rows*dim)
			e := make([]int8, dim)
			for i := range c {
				switch it % 4 {
				case 0:
					c[i] = int8(rng.Intn(256))
				case 1:
					c[i] = -1 // stored byte 255 = code +127
				case 2:
					c[i] = 0 // stored byte 0 = code -128
				default:
					c[i] = int8([]int{0, 1, 127, 128, 129, 255}[rng.Intn(6)])
				}
			}
			for i := range e {
				switch it % 3 {
				case 0:
					e[i] = int8(rng.Intn(127) - 63)
				case 1:
					e[i] = 63
				default:
					e[i] = -63
				}
			}
			qq := &q8query{e: e}
			var sum int32
			for _, v := range e {
				sum += int32(v)
			}
			qq.bias = 128 * sum
			want := make([]int32, rows)
			for r := range want {
				want[r] = dotQ8Go(c[r*dim:(r+1)*dim], qq)
			}
			got := make([]int32, rows)
			dotQ8Block(c, qq, got)
			for r := range want {
				if got[r] != want[r] {
					t.Fatalf("block dim=%d it=%d row=%d: got %d want %d", dim, it, r, got[r], want[r])
				}
				if d := dotQ8(c[r*dim:(r+1)*dim], qq); d != want[r] {
					t.Fatalf("single dim=%d it=%d row=%d: got %d want %d", dim, it, r, d, want[r])
				}
			}
			idx := make([]int32, 1+rng.Intn(2*rows))
			for i := range idx {
				idx[i] = int32(rng.Intn(rows))
			}
			gotR := make([]int32, len(idx))
			dotQ8Rows(c, idx, qq, gotR)
			for i, x := range idx {
				if gotR[i] != want[x] {
					t.Fatalf("rows dim=%d it=%d i=%d row=%d: got %d want %d", dim, it, i, x, gotR[i], want[x])
				}
			}
		}
	}
}
