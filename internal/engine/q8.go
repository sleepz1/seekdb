package engine

import "math"

// Int8 pruning copy.
//
// Each committed vector x also gets an int8 copy c with a per-row scale s
// (x ~ s*c, |c| <= 127), its exact square sum C2 = sum c^2 and a residual
// bound r >= ||x - s*c|| (computed in float64, rounded up). A query q is
// quantised the same way to int16 e with scale t (residual rho). Then
//
//	||x - q|| >= ||s*c - t*e|| - r - rho                (triangle inequality)
//	||s*c - t*e||^2 = s^2*C2 - 2*s*t*D + t^2*E2,  D = sum c*e (exact int)
//
// D is computed with AVX2 integer multiply-adds (16 dimensions per
// instruction, a quarter of the bytes of float32), the rest in float64 with
// an explicit error allowance. A row is skipped only when this lower bound
// is strictly above the current k-th exact distance; every other row is
// recomputed exactly in float64, so results are unchanged.

type q8meta struct {
	s  float32 // scale (exactly representable, used as float64)
	c2 int32   // sum of c^2
	r  float32 // residual bound
}

// useQ8 reports whether the table keeps the int8 copy: AVX2 is needed, and
// the dimension must be a multiple of 16 and at most 512 (so the int32 dot
// product cannot overflow: 512*127*32767 < 2^31).
func (t *Table) useQ8() bool { return hasAVX2 && t.vdim%16 == 0 && t.vdim <= 512 }

func roundUp32(x float64) float32 {
	f := float32(x)
	if float64(f) < x {
		f = math.Nextafter32(f, float32(math.Inf(1)))
	}
	return f
}

// encodeQ8 fills dst with the int8 copy of src and returns its metadata.
func encodeQ8(dst []int8, src []float32) q8meta {
	var mx float64
	for _, f := range src {
		if a := math.Abs(float64(f)); a > mx {
			mx = a
		}
	}
	if mx == 0 {
		for i := range dst {
			dst[i] = 0
		}
		return q8meta{}
	}
	s := float64(float32(mx / 127))
	if s == 0 { // mx below the smallest float32 scale: keep exact zeros, residual = ||x||
		var n float64
		for i, f := range src {
			dst[i] = 0
			n += float64(f) * float64(f)
		}
		return q8meta{r: roundUp32(math.Sqrt(n)*(1+1e-6) + 1e-300)}
	}
	var c2 int32
	var res float64
	for i, f := range src {
		c := math.Round(float64(f) / s)
		if c > 127 {
			c = 127
		} else if c < -127 {
			c = -127
		}
		dst[i] = int8(c)
		c2 += int32(c * c)
		d := float64(f) - s*c
		res += d * d
	}
	return q8meta{s: float32(s), c2: c2, r: roundUp32(math.Sqrt(res)*(1+1e-6) + 1e-300)}
}

// q8query is a query vector quantised to int16.
type q8query struct {
	e   []int16
	t   float64
	e2  float64 // sum e^2 (exact in float64)
	rho float64 // bound of ||q - t*e||
}

func prepQ8(q []float32) q8query {
	var mx float64
	for _, f := range q {
		if a := math.Abs(float64(f)); a > mx {
			mx = a
		}
	}
	e := make([]int16, len(q))
	if mx == 0 {
		return q8query{e: e, t: 1}
	}
	t := mx / 32767
	var e2, res float64
	for i, f := range q {
		c := math.Round(float64(f) / t)
		if c > 32767 {
			c = 32767
		} else if c < -32767 {
			c = -32767
		}
		e[i] = int16(c)
		e2 += c * c
		d := float64(f) - t*c
		res += d * d
	}
	return q8query{e: e, t: t, e2: e2, rho: math.Sqrt(res)*(1+1e-6) + 1e-300}
}

// q8prune reports whether a row with metadata m and dot product d is
// provably farther than kth from the query.
func (qq *q8query) prune(m q8meta, d int32, kth float64) bool {
	s := float64(m.s)
	a := s * s * float64(m.c2)
	b := 2 * s * qq.t * float64(d)
	c := qq.t * qq.t * qq.e2
	val := a - b + c
	// Each product/sum above is within a few ulps; allow 2^-40 of the
	// magnitude, far more than the float64 rounding error.
	val -= (a + math.Abs(b) + c) * (1.0 / (1 << 40))
	thr := kth + float64(m.r) + qq.rho
	return val > 0 && val > thr*thr && !math.IsInf(thr, 1)
}

// closest returns the indices of the (at most) k rows with the smallest
// estimated squared distance ||s*c - t*e||^2 (selection only; not a bound).
func (qq *q8query) closest(ms []q8meta, dots []int32, k int) []int {
	type cand struct {
		est float64
		j   int
	}
	best := make([]cand, 0, k+1)
	c := qq.t * qq.t * qq.e2
	for j := range dots {
		s := float64(ms[j].s)
		est := s*s*float64(ms[j].c2) - 2*s*qq.t*float64(dots[j]) + c
		if len(best) == k && est >= best[k-1].est {
			continue
		}
		i := len(best)
		best = append(best, cand{})
		for i > 0 && best[i-1].est > est {
			best[i] = best[i-1]
			i--
		}
		best[i] = cand{est, j}
		if len(best) > k {
			best = best[:k]
		}
	}
	out := make([]int, len(best))
	for i, b := range best {
		out[i] = b.j
	}
	return out
}

func dotQ8Go(c []int8, e []int16) int32 {
	var s int32
	for i := range c {
		s += int32(c[i]) * int32(e[i])
	}
	return s
}
