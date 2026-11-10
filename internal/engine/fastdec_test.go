package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func TestFastDecimalMatchesStrconv(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	cases := []string{"0", "-0", "+0", "-0.000000", "1.", ".5", "-.5", "007", "123456789012345", "1234567890123456",
		"0.1", "0.2", "0.3", "-1.000001", "999999.999999", "1e5", ".", "-", "+", "1.2.3", "--1", "0.0000000000000000000001"}
	for i := 0; i < 300000; i++ {
		switch i % 3 {
		case 0:
			cases = append(cases, fmt.Sprintf("%.6f", rng.Float64()*2-1))
		case 1:
			cases = append(cases, strconv.FormatFloat(rng.NormFloat64()*math.Pow(10, float64(rng.Intn(16)-6)), 'f', rng.Intn(12), 64))
		default:
			cases = append(cases, fmt.Sprintf("%d.%0*d", rng.Intn(1000), rng.Intn(14)+1, rng.Int63n(1e12)))
		}
	}
	for _, s := range cases {
		want, werr := strconv.ParseFloat(s, 64)
		got, ok := fastDecimal(s)
		if !ok {
			continue
		}
		if werr != nil || math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("%q: fast %v (%x), strconv %v (%x) err %v", s, got, math.Float64bits(got), want, math.Float64bits(want), werr)
		}
	}
	// The in-place vector parser agrees with the split-based one.
	for _, s := range []string{"[1,2,3]", "[ 1 , 2 ,3 ]", "[1,2]", "[1,2,3,4]", "[1,,3]", "[1,2,]", "[]", "[1e40,1,1]", "[a,1,1]"} {
		a, ea := parseVectorText(s, 3)
		inner := s[1 : len(s)-1]
		var b []float32
		var eb error
		if inner == "" {
			b, eb = parseVectorElems(nil, 3)
		} else {
			b, eb = parseVectorElems(splitComma(inner), 3)
		}
		if fmt.Sprint(a, ea) != fmt.Sprint(b, eb) {
			t.Fatalf("%s: %v %v vs %v %v", s, a, ea, b, eb)
		}
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func TestVectorEncodingRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	gens := []func() float32{
		func() float32 { return float32(rng.Float64()*2 - 1) },
		func() float32 { f, _ := strconv.ParseFloat(fmt.Sprintf("%.6f", rng.Float64()*2-1), 64); return float32(f) },
		func() float32 { f, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", rng.NormFloat64()*100), 64); return float32(f) },
		func() float32 { return float32(rng.Intn(5)) },
		func() float32 { return float32(math.Copysign(0, -1)) },
		func() float32 { return float32(rng.NormFloat64() * 1e30) },
		func() float32 { return math.Float32frombits(rng.Uint32() &^ (0xff << 23) | uint32(rng.Intn(254)+1)<<23) },
	}
	scaled := 0
	for it := 0; it < 20000; it++ {
		g := gens[it%len(gens)]
		v := make([]float32, 1+rng.Intn(140))
		for i := range v {
			v[i] = g()
		}
		enc := appendVector(nil, v)
		if enc[0] != 0 {
			scaled++
		}
		got := make([]float32, len(v))
		n, err := decodeVector(got, append(enc, 7, 7))
		if err != nil || n != len(enc) {
			t.Fatalf("decode: %v n=%d len=%d", err, n, len(enc))
		}
		for i := range v {
			if math.Float32bits(got[i]) != math.Float32bits(v[i]) {
				t.Fatalf("element %d: %v -> %v (tag %d)", i, v[i], got[i], enc[0])
			}
		}
	}
	if scaled < 5000 {
		t.Fatalf("scaled encoding used only %d times", scaled)
	}
}
