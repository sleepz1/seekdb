//go:build arm64

package engine

import (
	"encoding/binary"
	"os"
)

// On arm64 the int8 scan uses NEON (part of the base instruction set), with
// the ARMv8.2 dot-product instruction when the CPU has it; the float32
// estimate and the prune test use the portable Go code.

func hasQ8Kernel() bool { return true }

// hasDotProd reports the asimddp feature (HWCAP bit 20) from the auxiliary
// vector. When /proc is not readable the base NEON kernel is used.
var hasDotProd = func() bool {
	if os.Getenv("MVDB_NODOT") != "" {
		return false // experiments only
	}
	b, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return false
	}
	const atHWCAP, asimddp = 16, 1 << 20
	for i := 0; i+16 <= len(b); i += 16 {
		if binary.LittleEndian.Uint64(b[i:]) == atHWCAP {
			return binary.LittleEndian.Uint64(b[i+8:])&asimddp != 0
		}
	}
	return false
}()

func l2sq32(a, b []float32) float32 { return l2sq32Go(a, b) }

//go:noescape
func dotS8Block(c *int8, e *int8, dim int, rows int, out *int32)

//go:noescape
func dotS8Rows(c *int8, idx *int32, n int, e *int8, dim int, out *int32)

//go:noescape
func dotSDBlock(c *int8, e *int8, dim int, rows int, out *int32)

//go:noescape
func dotSDRows(c *int8, idx *int32, n int, e *int8, dim int, out *int32)

// dotQ8Block sets out[r] to the exact dot product of code row r of c with
// the query codes (see q8.go for the stored form).
func dotQ8Block(c []int8, qq *q8query, out []int32) {
	dim := len(qq.e)
	if len(out) == 0 || dim == 0 {
		return
	}
	if dim%16 != 0 || len(c) < len(out)*dim {
		panic("dotQ8Block: bad shape")
	}
	if hasDotProd {
		dotSDBlock(&c[0], &qq.e[0], dim, len(out), &out[0])
	} else {
		dotS8Block(&c[0], &qq.e[0], dim, len(out), &out[0])
	}
}

// dotQ8Rows is dotQ8Block for the rows idx[i] of the chunk c.
func dotQ8Rows(c []int8, idx []int32, qq *q8query, out []int32) {
	dim := len(qq.e)
	if len(idx) == 0 || dim == 0 {
		return
	}
	if dim%16 != 0 || len(out) < len(idx) {
		panic("dotQ8Rows: bad shape")
	}
	rows := len(c) / dim
	for _, x := range idx {
		if x < 0 || int(x) >= rows {
			panic("dotQ8Rows: row out of range")
		}
	}
	if hasDotProd {
		dotSDRows(&c[0], &idx[0], len(idx), &qq.e[0], dim, &out[0])
	} else {
		dotS8Rows(&c[0], &idx[0], len(idx), &qq.e[0], dim, &out[0])
	}
}

func dotQ8(c []int8, qq *q8query) int32 {
	var out [1]int32
	dotQ8Block(c, qq, out[:])
	return out[0]
}

func pruneMask(s []float32, c2 []int32, r []float32, d []int32, k *pruneK, out []byte) {
	pruneMaskGo(s, c2, r, d, k, out)
}
