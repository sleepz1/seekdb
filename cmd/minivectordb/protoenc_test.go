package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"testing"

	"minivectordb/internal/engine"
)

// The direct encoder must produce exactly the bytes of the encoding/json
// writer for every kind of value.
func TestFastResultMatchesJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	randStr := func() string {
		n := rng.Intn(24)
		b := make([]byte, 0, n*3)
		for i := 0; i < n; i++ {
			switch rng.Intn(8) {
			case 0:
				b = append(b, byte(rng.Intn(256))) // any byte, incl. invalid UTF-8
			case 1:
				b = append(b, []byte(string(rune(rng.Intn(0x11000))))...)
			case 2:
				b = append(b, "\u2028\u2029\"\\/<>&\x00\x1f\x7f\b\f\n\r\t"[rng.Intn(20)])
			case 3:
				b = append(b, "中文字符串"...)
			case 4:
				b = append(b, []byte{0xe2, 0x80, byte(0xa8 + rng.Intn(2))}...)
			default:
				b = append(b, byte(0x20+rng.Intn(0x5f)))
			}
		}
		return string(b)
	}
	floats := []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 1e-6, 9.999999e-7, 1e-7, 1e21, 9.99e20, 1e22, 1e100, 1e-100,
		math.MaxFloat64, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), math.Inf(-1), 5, 0.10000000149011612, 123456789.125,
		float64(math.MaxFloat32), float64(math.SmallestNonzeroFloat32), 1e-5, 1.5e-9, -2.5e-11, 3e20, 1e6, 1e15, 1e16, 1e17}
	randF := func() float64 {
		switch rng.Intn(4) {
		case 0:
			return floats[rng.Intn(len(floats))]
		case 1:
			return float64(float32(rng.NormFloat64()))
		case 2:
			return math.Float64frombits(rng.Uint64())
		}
		return rng.NormFloat64() * math.Pow(10, float64(rng.Intn(60)-30))
	}
	randVal := func() engine.Value {
		switch rng.Intn(7) {
		case 0:
			return engine.IntVal(int64(rng.Uint64()))
		case 1:
			return engine.Value{K: engine.KBool, I: int64(rng.Intn(2))}
		case 2:
			return engine.FloatVal(randF())
		case 3:
			return engine.StrVal(randStr())
		case 4:
			v := make([]float32, rng.Intn(6))
			for i := range v {
				v[i] = float32(randF())
			}
			if rng.Intn(4) == 0 {
				v = nil
			}
			return engine.Value{K: engine.KVec, V: v}
		case 5:
			return engine.Value{} // NULL
		}
		return engine.IntVal(int64(rng.Intn(100)) - 50)
	}
	ids := []string{"null", "1", "-7", "9223372036854775807", "1.5e3", `"a-7"`, `"with space"`, `"\u00e9\"q"`, "true", `""`}
	for it := 0; it < 20000; it++ {
		r := &engine.Result{IsQuery: rng.Intn(5) != 0, Affected: int64(rng.Uint64() >> uint(rng.Intn(64)))}
		if r.IsQuery {
			nc := rng.Intn(5)
			if nc > 0 || rng.Intn(2) == 0 {
				r.Columns = make([]string, nc)
			}
			for i := range r.Columns {
				r.Columns[i] = randStr()
			}
			nr := rng.Intn(4)
			if nr > 0 || rng.Intn(2) == 0 {
				r.Rows = make([][]engine.Value, nr)
			}
			for i := range r.Rows {
				r.Rows[i] = make([]engine.Value, nc)
				for j := range r.Rows[i] {
					r.Rows[i][j] = randVal()
				}
			}
		}
		id := ids[rng.Intn(len(ids))]
		var want bytes.Buffer
		bw := bufio.NewWriter(&want)
		writeProtoResult(bw, json.RawMessage(id), r)
		bw.Flush()
		got := appendProtoResult(nil, []byte(id), r)
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("it %d:\n got %q\nwant %q", it, got, want.Bytes())
		}
	}
}
