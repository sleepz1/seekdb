//go:build !amd64

package engine

func l2sq32(a, b []float32) float32 { return l2sq32Go(a, b) }

const hasAVX2 = false

func dotQ8Block(c []int8, e []int16, out []int32) { panic("unreachable") }

func dotQ8(c []int8, e []int16) int32 { return dotQ8Go(c, e) }
