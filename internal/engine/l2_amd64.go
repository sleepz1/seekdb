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

// dotQ8Block sets out[r] = sum_d c[r*dim+d]*e[d] for r < len(out), dim =
// len(e) (a multiple of 16). Requires hasAVX2.
//
//go:noescape
func dotQ8Block(c []int8, e []int16, out []int32)

func dotQ8(c []int8, e []int16) int32 {
	var out [1]int32
	dotQ8Block(c, e, out[:])
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
