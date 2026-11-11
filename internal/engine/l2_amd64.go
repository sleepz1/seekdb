//go:build amd64

package engine

// l2sqAVX returns the float32 sum of squared differences of a and b[:len(a)]
// using 256-bit AVX lanes. It is only an estimate used for pruning; every
// accepted candidate is recomputed exactly in float64 (l2sqBounded).
//
//go:noescape
func l2sqAVX(a, b []float32) float32

func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)
func xgetbv() (eax, edx uint32)

// dotU8Block sets out[r] = sum_d u[r*dim+d]*e[d] - bias for r < len(out),
// where u is the stored code read as an unsigned byte and dim = len(e) (a
// multiple of 16, |e[d]| <= 63). Requires hasAVX2.
//
//go:noescape
func dotU8Block(c []int8, e []int8, bias int32, out []int32)

// dotU8Rows is dotU8Block for the rows idx[i] of the chunk c.
//
//go:noescape
func dotU8Rows(c []int8, idx []int32, e []int8, bias int32, out []int32)

// dotQ8Block sets out[r] to the exact dot product of code row r of c with
// the query codes (see q8.go for the stored form).
func dotQ8Block(c []int8, qq *q8query, out []int32) { dotU8Block(c, qq.e, qq.bias, out) }

// dotQ8Rows is dotQ8Block for the rows idx[i] of the chunk c.
func dotQ8Rows(c []int8, idx []int32, qq *q8query, out []int32) {
	dotU8Rows(c, idx, qq.e, qq.bias, out)
}

// pruneMask is pruneMaskGo with four rows per step (AVX2, float64, the same
// operations in the same order, so the decisions are bit-identical).
// len(d) must be a multiple of 4; s, c2, r hold at least len(d) entries and
// out at least len(d)/4. Requires hasAVX2.
//
//go:noescape
func pruneMask(s []float32, c2 []int32, r []float32, d []int32, k *pruneK, out []byte)

func dotQ8(c []int8, qq *q8query) int32 {
	var out [1]int32
	dotU8Block(c, qq.e, qq.bias, out[:])
	return out[0]
}

var hasAVX2 = func() bool {
	if !hasAVX {
		return false
	}
	_, b, _, _ := cpuid(7, 0)
	return b&(1<<5) != 0
}()

var hasAVX = func() bool {
	_, _, c, _ := cpuid(1, 0)
	const osxsave, avx = 1 << 27, 1 << 28
	if c&osxsave == 0 || c&avx == 0 {
		return false
	}
	a, _ := xgetbv()
	return a&6 == 6 // XMM and YMM state enabled by the OS
}()

func l2sq32(a, b []float32) float32 {
	if hasAVX {
		return l2sqAVX(a, b)
	}
	return l2sq32Go(a, b)
}
