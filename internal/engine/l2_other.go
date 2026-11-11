//go:build !amd64

package engine

func l2sq32(a, b []float32) float32 { return l2sq32Go(a, b) }

const hasAVX2 = false

func dotQ8Block(c []int8, qq *q8query, out []int32) { panic("unreachable") }

func dotQ8(c []int8, qq *q8query) int32 { return dotQ8Go(c, qq) }

func dotQ8Rows(c []int8, idx []int32, qq *q8query, out []int32) { dotQ8RowsGo(c, idx, qq, out) }

func pruneMask(s []float32, c2 []int32, r []float32, d []int32, k *pruneK, out []byte) {
	pruneMaskGo(s, c2, r, d, k, out)
}
