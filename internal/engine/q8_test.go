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
			// worst case extreme codes for the saturation bound: stored
			// byte 255 with query code +63 or -63 in every position
			if it < 2 {
				var sum int32
				for i := range c {
					c[i] = -1
					qq.e[i] = int8(63 - 126*it)
					sum += int32(qq.e[i])
				}
				qq.bias = 128 * sum
			}
			if got, want := dotQ8(c, &qq), dotQ8Go(c, &qq); got != want {
				t.Fatalf("dim %d: dot %d want %d", dim, got, want)
			}
			if it < 2 {
				continue
			}
			for i := range qq.e {
				if qq.e[i] > 63 || qq.e[i] < -63 {
					t.Fatalf("query code %d out of range", qq.e[i])
				}
			}
			var trueSq float64
			for i := range x {
				d := float64(x[i]) - float64(q[i])
				trueSq += d * d
			}
			dist := math.Sqrt(trueSq)
			// prune must never fire at kth >= true distance
			for _, kth := range []float64{dist, dist * (1 + 1e-12), dist * 1.001} {
				if qq.prune(m, dotQ8(c, &qq), kth) {
					t.Fatalf("dim %d scale %g: pruned a row at distance %g with kth %g", dim, sc, dist, kth)
				}
			}
		}
	}
	// block kernel == portable dot product, every row count (4-row and
	// 1-row paths) and every supported kind of dimension
	for _, dim := range []int{16, 32, 48, 128, 144, 512} {
		codes := make([]int8, 300*dim)
		for i := range codes {
			codes[i] = int8(rng.Intn(256))
		}
		q := make([]float32, dim)
		for i := range q {
			q[i] = float32(rng.NormFloat64())
		}
		qq := prepQ8(q)
		for _, rows := range []int{0, 1, 2, 3, 4, 5, 7, 8, 255, 256, 300} {
			out := make([]int32, rows)
			dotQ8Block(codes[:rows*dim], &qq, out)
			for r := range out {
				if want := dotQ8Go(codes[r*dim:(r+1)*dim], &qq); out[r] != want {
					t.Fatalf("dim %d rows %d: block row %d = %d, want %d", dim, rows, r, out[r], want)
				}
			}
		}
	}
}

// The prune kernel must make exactly the decisions of q8query.prune, and the
// indexed dot kernel must equal the portable dot product.
func TestPruneMaskMatchesPrune(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	const n = 1024
	s := make([]float32, n)
	c2 := make([]int32, n)
	r := make([]float32, n)
	d := make([]int32, n)
	for it := 0; it < 400; it++ {
		qq := q8query{t: math.Exp(rng.NormFloat64() * 4), e2: float64(rng.Int63n(1 << 36)), rho: math.Exp(rng.NormFloat64()*4 - 8)}
		for i := range s {
			s[i] = float32(math.Exp(rng.NormFloat64() * 4))
			c2[i] = rng.Int31n(128 * 127 * 127)
			d[i] = rng.Int31() - 1<<30
			r[i] = float32(math.Exp(rng.NormFloat64()*4 - 8))
			switch rng.Intn(40) {
			case 0:
				s[i], c2[i], d[i] = 0, 0, 0
			case 1:
				r[i] = float32(math.Inf(1))
			case 2:
				s[i] = math.MaxFloat32
			case 3:
				d[i] = math.MaxInt32
			case 4:
				d[i] = math.MinInt32
			}
		}
		// a k-th distance near the estimates, so that both outcomes occur
		est := float64(s[0])*float64(s[0])*float64(c2[0]) + qq.t*qq.t*qq.e2
		for _, kth := range []float64{0, math.Sqrt(est) * rng.Float64() * 2, math.Sqrt(est), 1e-300, 1e300, math.MaxFloat64} {
			k := qq.consts(kth)
			got := make([]byte, n/4)
			want := make([]byte, n/4)
			pruneMask(s, c2, r, d, &k, got)
			pruneMaskGo(s, c2, r, d, &k, want)
			pruned := 0
			for i := 0; i < n; i++ {
				w := qq.prune(q8meta{s: s[i], c2: c2[i], r: r[i]}, d[i], kth)
				if w {
					pruned++
				}
				if g := got[i/4]&(1<<(i%4)) != 0; g != w {
					t.Fatalf("it %d row %d kth %g: kernel %v, prune %v", it, i, kth, g, w)
				}
				if g := want[i/4]&(1<<(i%4)) != 0; g != w {
					t.Fatalf("it %d row %d kth %g: portable %v, prune %v", it, i, kth, g, w)
				}
			}
			_ = pruned
		}
	}
}

func TestDotQ8Rows(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	for _, dim := range []int{16, 32, 48, 128, 512} {
		codes := make([]int8, 1024*dim)
		for i := range codes {
			codes[i] = int8(rng.Intn(256))
		}
		q := make([]float32, dim)
		for i := range q {
			q[i] = float32(rng.NormFloat64())
		}
		qq := prepQ8(q)
		idx := []int32{0, 1023, 5, 5, 700}
		for i := 0; i < 250; i++ {
			idx = append(idx, int32(rng.Intn(1024)))
		}
		got := make([]int32, len(idx))
		want := make([]int32, len(idx))
		dotQ8Rows(codes, idx, &qq, got)
		dotQ8RowsGo(codes, idx, &qq, want)
		for i := range idx {
			if got[i] != want[i] {
				t.Fatalf("dim %d row %d: %d want %d", dim, idx[i], got[i], want[i])
			}
		}
		dotQ8Rows(codes, nil, &qq, nil)
	}
}
