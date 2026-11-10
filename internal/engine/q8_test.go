package engine

import (
	"math"
	"math/rand"
	"testing"
)

func TestQ8KernelAndBound(t *testing.T) {
	if !hasAVX2 {
		t.Skip("no AVX2")
	}
	rng := rand.New(rand.NewSource(9))
	scales := []float64{1, 1e-3, 1e3, 1e-30, 1e30}
	for _, dim := range []int{16, 32, 48, 128, 512} {
		for it := 0; it < 3000; it++ {
			sc := scales[it%len(scales)]
			x := make([]float32, dim)
			q := make([]float32, dim)
			for i := range x {
				x[i] = float32(rng.NormFloat64() * sc)
				q[i] = float32(rng.NormFloat64() * sc)
				if it%7 == 0 {
					q[i] = x[i] + float32(rng.NormFloat64()*sc*1e-4) // near duplicates
				}
			}
			if it%11 == 0 {
				x[rng.Intn(dim)] = float32(sc * 1e3) // outlier dominates the scale
			}
			c := make([]int8, dim)
			m := encodeQ8(c, x)
			qq := prepQ8(q)
			// worst case extreme codes for the overflow bound
			if it == 0 {
				for i := range c {
					c[i] = -127
					qq.e[i] = 32767
				}
			}
			if got, want := dotQ8(c, qq.e), dotQ8Go(c, qq.e); got != want {
				t.Fatalf("dim %d: dot %d want %d", dim, got, want)
			}
			if it == 0 {
				continue
			}
			var trueSq float64
			for i := range x {
				d := float64(x[i]) - float64(q[i])
				trueSq += d * d
			}
			dist := math.Sqrt(trueSq)
			// prune must never fire at kth >= true distance
			for _, kth := range []float64{dist, dist * (1 + 1e-12), dist * 1.001} {
				if qq.prune(m, dotQ8(c, qq.e), kth) {
					t.Fatalf("dim %d scale %g: pruned a row at distance %g with kth %g", dim, sc, dist, kth)
				}
			}
		}
	}
	// block kernel == per-row
	dim := 128
	codes := make([]int8, 300*dim)
	for i := range codes {
		codes[i] = int8(rng.Intn(255) - 127)
	}
	e := make([]int16, dim)
	for i := range e {
		e[i] = int16(rng.Intn(65535) - 32767)
	}
	out := make([]int32, 300)
	dotQ8Block(codes, e, out)
	for r := range out {
		if out[r] != dotQ8Go(codes[r*dim:(r+1)*dim], e) {
			t.Fatalf("block row %d", r)
		}
	}
}
