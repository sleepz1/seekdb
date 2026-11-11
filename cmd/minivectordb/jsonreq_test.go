package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"minivectordb/internal/engine"
)

// Random JSON documents, valid and mutated, decoded by parseRequest (via
// decodeRequest) and by json.Unmarshal.
func TestParseRequestMatchesJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keys := []string{"id", "op", "sql", "search_mode", "ID", "Id", "OP", "Sql", "SEARCH_MODE", "Search_Mode",
		"\u017fql", "\u017fearch_mode", "i\u0064", "x", "", "idd", "search-mode", "\u212aid", "\\u0069d", "s\\u0071l", "\\ud800id", "\xffop"}
	strs := []string{"execute", "shutdown", "exact", "ann", "", "SELECT 1", `a\"b`, `\n\t\\`, `\u00e9\u4f60`, `\ud83d\ude00`,
		`\ud800`, `\udc00x`, `\ud800\u0041`, "\xff\xfe", "é你", `\/`, `\b\f\r`, "\u2028"}
	var value func(depth int) string
	value = func(depth int) string {
		switch rng.Intn(9) {
		case 0:
			return "null"
		case 1:
			return []string{"true", "false"}[rng.Intn(2)]
		case 2:
			return []string{"0", "-1", "1.5", "-0.0e+10", "12345678901234567890", "1E-3", "7"}[rng.Intn(7)]
		case 3, 4, 5:
			return `"` + strs[rng.Intn(len(strs))] + `"`
		case 6:
			if depth > 3 {
				return "1"
			}
			n := rng.Intn(3)
			parts := make([]string, n)
			for i := range parts {
				parts[i] = value(depth + 1)
			}
			return "[" + strings.Join(parts, ",") + "]"
		default:
			if depth > 3 {
				return `"x"`
			}
			return object(rng, keys, value, depth+1)
		}
	}
	ws := []string{"", "", " ", "\n", "\t ", "\r\n"}
	for n := 0; n < 60000; n++ {
		var doc string
		switch rng.Intn(10) {
		case 0:
			doc = value(0)
		default:
			doc = object(rng, keys, value, 0)
		}
		doc = ws[rng.Intn(len(ws))] + doc + ws[rng.Intn(len(ws))]
		b := []byte(doc)
		if rng.Intn(3) == 0 && len(b) > 0 { // mutate
			alpha := []byte("{}[]\":,1a\\ \t-.eE+0nulltrfs\xff\x01")
			for k := rng.Intn(3); k >= 0 && len(b) > 0; k-- {
				p := rng.Intn(len(b))
				switch rng.Intn(3) {
				case 0:
					b[p] = alpha[rng.Intn(len(alpha))]
				case 1:
					b = append(b[:p], b[p+1:]...)
				default:
					b = append(b[:p], append([]byte{alpha[rng.Intn(len(alpha))]}, b[p:]...)...)
				}
			}
		}
		checkDecode(t, b)
	}
	// nesting depth limit of encoding/json
	for _, d := range []int{9999, 10000, 10001} {
		doc := `{"id":` + strings.Repeat("[", d) + strings.Repeat("]", d) + `,"op":"x"}`
		checkDecode(t, []byte(doc))
	}
}

func object(rng *rand.Rand, keys []string, value func(int) string, depth int) string {
	n := rng.Intn(6)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"` + keys[rng.Intn(len(keys))] + `":` + value(depth)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// The protocol writers must produce the bytes encoding/json's Encoder (HTML
// escaping off) produced for the same structs, ids compacted as RawMessage.
func TestProtoWritersMatchJSON(t *testing.T) {
	ids := []string{"null", "1", " 2 ", `"a b"`, "[ 1 , 2 ]", "{ \"k\" : [ true , null ] }", `"\u00e9 \"q\""`, "-1.5e3", "\n\t7\r"}
	msgs := []string{"", "Duplicate entry '1' for key 'PRIMARY'", "a\"b\\c\n\x01\u2028<&>", "\xff bad utf8", "中文"}
	for _, id := range ids {
		for _, m := range msgs {
			for _, code := range []string{"", "duplicate_key"} {
				var got, want bytes.Buffer
				gw := bufio.NewWriter(&got)
				writeProtoError(gw, []byte(id), code, m)
				gw.Flush()
				c := code
				if c == "" {
					c = "internal_error"
				}
				refEncode(&want, struct {
					ID    json.RawMessage `json:"id"`
					OK    bool            `json:"ok"`
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}{ID: json.RawMessage(id), Error: struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}{c, m}})
				if got.String() != want.String() {
					t.Fatalf("error id %q msg %q:\n got %q\nwant %q", id, m, got.String(), want.String())
				}
			}
		}
		var got, want bytes.Buffer
		gw := bufio.NewWriter(&got)
		writeProtoShutdown(gw, []byte(id))
		gw.Flush()
		refEncode(&want, struct {
			ID json.RawMessage `json:"id"`
			OK bool            `json:"ok"`
		}{json.RawMessage(id), true})
		if got.String() != want.String() {
			t.Fatalf("shutdown id %q:\n got %q\nwant %q", id, got.String(), want.String())
		}
		got.Reset()
		want.Reset()
		gw = bufio.NewWriter(&got)
		r := &engine.Result{IsQuery: true, Columns: []string{"id", "d"}, Rows: [][]engine.Value{{engine.IntVal(1), engine.FloatVal(0.5)}}}
		writeProtoResult(gw, []byte(id), r)
		gw.Flush()
		refEncode(&want, struct {
			ID      json.RawMessage `json:"id"`
			OK      bool            `json:"ok"`
			Columns []string        `json:"columns"`
			Rows    [][]any         `json:"rows"`
		}{json.RawMessage(id), true, []string{"id", "d"}, [][]any{{int64(1), 0.5}}})
		if got.String() != want.String() {
			t.Fatalf("result id %q:\n got %q\nwant %q", id, got.String(), want.String())
		}
	}
}

func refEncode(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// tsvCell renders floats and vectors as json.Marshal did.
func TestTSVCellMatchesJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	special := []float64{0, math.Copysign(0, -1), 1e-7, 9.999e-7, 1e21, 1e20, 0.1, 5, math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.NaN(), math.Inf(1), float64(math.MaxFloat32), 1e-45, 3.4e38}
	for i := 0; i < 20000; i++ {
		var f float64
		if i < len(special) {
			f = special[i]
		} else {
			f = math.Float64frombits(rng.Uint64())
			if rng.Intn(2) == 0 {
				f = rng.NormFloat64() * math.Pow(10, float64(rng.Intn(60)-30))
			}
		}
		want := "null"
		if b, err := json.Marshal(f); err == nil {
			want = string(b)
		}
		if got := tsvCell(engine.FloatVal(f)); got != want {
			t.Fatalf("float %v: got %s want %s", f, got, want)
		}
		v := make([]float32, rng.Intn(4))
		for j := range v {
			v[j] = float32(f) * float32(j+1)
			if rng.Intn(3) == 0 {
				v[j] = math.Float32frombits(rng.Uint32())
			}
		}
		if rng.Intn(50) == 0 {
			v = nil
		}
		want = "null"
		if b, err := json.Marshal(v); err == nil {
			want = string(b)
		}
		if got := tsvCell(engine.Value{K: engine.KVec, V: v}); got != want {
			t.Fatalf("vector %v: got %s want %s", v, got, want)
		}
	}
}

// parseFlags accepts what package flag accepted, with the same values.
func TestParseFlagsMatchesFlag(t *testing.T) {
	cases := [][]string{
		{}, {"-data", "/x"}, {"--data-dir", "/y"}, {"-data=/z"}, {"--data-dir=/w", "--format", "tsv"},
		{"--format=json", "-mode", "ann", "-cache-mb", "7"}, {"-cache-mb=0x10"}, {"-cache-mb", "abc"},
		{"-ready-line", "READY", "-data", "/d"}, {"-unknown"}, {"-data"}, {"positional", "-data", "/x"},
		{"-data", "/x", "rest", "-mode", "ann"}, {"--", "-data", "/x"}, {"-"}, {"---data", "/x"}, {"-=x"},
		{"-h"}, {"--help"}, {"-data", ""}, {"-data=", "-mode=exact"}, {"-cache-mb", "-1"}, {"-cache-mb=99999999999999999999"},
	}
	for _, args := range cases {
		fs := flag.NewFlagSet("minivectordb", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dataDir := fs.String("data-dir", "D", "")
		fs.StringVar(dataDir, "data", *dataDir, "")
		mode := fs.String("mode", "M", "")
		format := fs.String("format", "F", "")
		ready := fs.String("ready-line", "R", "")
		cache := fs.Int64("cache-mb", 0, "")
		errF := fs.Parse(args)
		o := cmdOptions{dataDir: "D", mode: "M", format: "F", ready: "R"}
		errP := parseFlags(args, &o)
		if (errF == nil) != (errP == nil) {
			t.Fatalf("%q: flag error %v, parseFlags error %v", args, errF, errP)
		}
		if errF == nil && (o.dataDir != *dataDir || o.mode != *mode || o.format != *format || o.ready != *ready || o.cacheMB != *cache) {
			t.Fatalf("%q: got %+v, flag gives %s %s %s %s %d", args, o, *dataDir, *mode, *format, *ready, *cache)
		}
	}
	_ = strconv.Itoa
}
