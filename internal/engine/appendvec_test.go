package engine

import (
	"encoding/binary"
	"math"
	"math/bits"
	"math/rand"
	"strconv"
	"testing"
)

// appendVectorRef is the encoder before the single-pass rewrite.
func appendVectorRef(buf []byte, v []float32) []byte {
	if p, ok := decimalScale(v); ok {
		// Bit-packed scaled integers: tag 16+p, width byte w, then the
		// values k + 2^(w-1) as w-bit little-endian fields. "0.123456"-style
		// data needs 21 bits instead of 24 (or 32 for raw float32).
		sc := exactPow10[p]
		var maxAbs uint32
		for _, f := range v {
			k := int32(math.Round(float64(f) * sc))
			if k < 0 {
				k = -k
			}
			if uint32(k) > maxAbs {
				maxAbs = uint32(k)
			}
		}
		w := uint(bits.Len32(maxAbs)) + 1
		buf = append(buf, byte(16+p), byte(w))
		off := uint64(1) << (w - 1)
		var acc uint64
		var nacc uint
		for _, f := range v {
			k := int64(math.Round(float64(f) * sc))
			acc |= uint64(k+int64(off)) << nacc
			nacc += w
			for nacc >= 8 {
				buf = append(buf, byte(acc))
				acc >>= 8
				nacc -= 8
			}
		}
		if nacc > 0 {
			buf = append(buf, byte(acc))
		}
		return buf
	}
	buf = append(buf, 0)
	for _, f := range v {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return buf
}

// decimalScale finds the smallest p (0..7) such that every element equals
// float32(k/10^p) bit-for-bit for an integer |k| < 2^23.

// The single-pass encoder must produce the same bytes as the old one.
func TestAppendVectorMatchesRef(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	gen := []func() float32{
		func() float32 { return float32(math.Round((rng.Float64()*2-1)*1e6) / 1e6) },
		func() float32 {
			v, _ := strconv.ParseFloat(strconv.FormatFloat(rng.Float64()*2-1, 'f', rng.Intn(8), 64), 64)
			return float32(v)
		},
		func() float32 { return float32(rng.Intn(200) - 100) },
		func() float32 { return float32(rng.NormFloat64()) },
		func() float32 { return math.Float32frombits(rng.Uint32()) },
		func() float32 { return float32(math.Copysign(0, -1)) },
		func() float32 { return float32(rng.Intn(1<<24) - 1<<23) },
		func() float32 { return float32(math.Round(rng.Float64()*8388607)) / 10 },
		func() float32 { return 0 },
	}
	for it := 0; it < 200000; it++ {
		dim := []int{1, 2, 3, 7, 16, 128, 1023, 1024, 1025, 2000}[rng.Intn(10)]
		if it%50 != 0 && dim > 128 {
			dim = 128
		}
		v := make([]float32, dim)
		g := gen[rng.Intn(len(gen))]
		for i := range v {
			v[i] = g()
			if rng.Intn(500) == 0 {
				v[i] = gen[rng.Intn(len(gen))]()
			}
		}
		got := appendVector([]byte{7}, v)
		hasNaN := false
		for _, f := range v {
			hasNaN = hasNaN || f != f
		}
		if hasNaN {
			// The old encoder could pack a quiet NaN as a garbage integer
			// (never reached: vector literals reject NaN). The new one
			// stores raw float32, which must decode bit for bit.
			back := make([]float32, dim)
			if _, err := decodeVector(back, got[1:]); err != nil {
				t.Fatal(err)
			}
			for i := range v {
				if math.Float32bits(back[i]) != math.Float32bits(v[i]) {
					t.Fatalf("NaN vector element %d: %x -> %x", i, math.Float32bits(v[i]), math.Float32bits(back[i]))
				}
			}
			continue
		}
		want := appendVectorRef([]byte{7}, v)
		if string(got) != string(want) {
			t.Fatalf("it %d dim %d: encodings differ\n got %x\nwant %x", it, dim, got[:min(len(got), 40)], want[:min(len(want), 40)])
		}
	}
	_ = binary.LittleEndian
	_ = bits.Len32
}

func benchVecs() [][]float32 {
	rng := rand.New(rand.NewSource(4))
	vs := make([][]float32, 256)
	for i := range vs {
		v := make([]float32, 128)
		for j := range v {
			f, _ := strconv.ParseFloat(strconv.FormatFloat(rng.Float64()*2-1, 'f', 6, 64), 64)
			v[j] = float32(f)
		}
		vs[i] = v
	}
	return vs
}

func BenchmarkAppendVector(b *testing.B) {
	vs := benchVecs()
	buf := make([]byte, 0, 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = appendVector(buf[:0], vs[i&255])
	}
}

func BenchmarkAppendVectorRef(b *testing.B) {
	vs := benchVecs()
	buf := make([]byte, 0, 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = appendVectorRef(buf[:0], vs[i&255])
	}
}

func BenchmarkParseVectorText(b *testing.B) {
	vs := benchVecs()
	txt := make([]string, len(vs))
	for i, v := range vs {
		s := "["
		for j, f := range v {
			if j > 0 {
				s += ","
			}
			s += strconv.FormatFloat(float64(f), 'f', 6, 32)
		}
		txt[i] = s + "]"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseVectorText(txt[i&255], 128); err != nil {
			b.Fatal(err)
		}
	}
}
