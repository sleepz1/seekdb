//go:build arm64

package engine

import (
	"math/rand"
	"testing"
)

// TestArm64KernelsAgree runs both arm64 kernels (base NEON and, when the
// CPU has it, SDOT) against the Go reference, whichever one is in use.
func TestArm64KernelsAgree(t *testing.T) {
	t.Logf("asimddp: %v", hasDotProd)
	rng := rand.New(rand.NewSource(5))
	for _, dim := range []int{16, 32, 48, 64, 80, 128, 192, 208, 512} {
		for it := 0; it < 100; it++ {
			rows := 1 + rng.Intn(20)
			c := make([]int8, rows*dim)
			e := make([]int8, dim)
			for i := range c {
				if it%3 == 0 {
					c[i] = int8([]int{0, 127, 128, 255}[rng.Intn(4)])
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
			qq := &q8query{e: e}
			idx := make([]int32, rows)
			for i := range idx {
				idx[i] = int32(rng.Intn(rows))
			}
			want := make([]int32, rows)
			for r := range want {
				want[r] = dotQ8Go(c[r*dim:(r+1)*dim], qq)
			}
			check := func(name string, got []int32, rowOf func(int) int) {
				for i := range got {
					if got[i] != want[rowOf(i)] {
						t.Fatalf("%s dim=%d it=%d i=%d: got %d want %d", name, dim, it, i, got[i], want[rowOf(i)])
					}
				}
			}
			same := func(i int) int { return i }
			byIdx := func(i int) int { return int(idx[i]) }
			got := make([]int32, rows)
			dotS8Block(&c[0], &e[0], dim, rows, &got[0])
			check("S8Block", got, same)
			dotS8Rows(&c[0], &idx[0], rows, &e[0], dim, &got[0])
			check("S8Rows", got, byIdx)
			if hasDotProd {
				dotSDBlock(&c[0], &e[0], dim, rows, &got[0])
				check("SDBlock", got, same)
				dotSDRows(&c[0], &idx[0], rows, &e[0], dim, &got[0])
				check("SDRows", got, byIdx)
			}
		}
	}
}
