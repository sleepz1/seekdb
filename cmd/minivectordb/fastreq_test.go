package main

import (
	"encoding/json"
	"math/rand"
	"testing"
)

func TestFastDecodeMatchesJSON(t *testing.T) {
	cases := []string{
		`{"id":1,"op":"execute","sql":"SELECT 1;","search_mode":"exact"}`,
		` { "id" : "a-7" , "op":"shutdown" } `,
		`{"id":null,"op":"execute","sql":"x"}`,
		`{"id":-1.5e3,"op":"execute","sql":"INSERT INTO t VALUES (1,'你好','[1,2]')"}`,
		`{"id":1,"op":"execute","sql":"a\"b"}`,
		`{"id":1,"op":"execute","sql":"a\nb"}`,
		`{"id":1,"op":"execute","sql":"tab	in"}`,
		`{"id":1,"ID":2,"op":"x"}`,
		`{"id":1,"id":2}`,
		`{"Op":"shutdown","id":3}`,
		`{"id":[1],"op":"x"}`,
		`{"id":{},"op":"x"}`,
		`{"id":1,"op":"execute","extra":{"a":1}}`,
		`{"id":01,"op":"x"}`,
		`{"id":1,"op":"x",}`,
		`{"id":1 "op":"x"}`,
		`{"id":1,"op":"x"} trailing`,
		`{"id":1,"op":5}`,
		`{"id":true,"op":"x"}`,
		`{}`,
		`[]`,
		`not json`,
		"{\"id\":1,\"op\":\"\xff\"}",
		`{"id":1,"op":"execute","sql":"SELECT 1","search_mode":"ann"}`,
		`{"id":12345678901234567890,"op":"execute"}`,
		`{"id":"x\u0041","op":"execute"}`,
		`{"id":1,"op":"execute","sql":null}`,
	}
	rng := rand.New(rand.NewSource(1))
	alpha := []byte(`{}[]":,1a\ 	-.e"o pidsqlnu`)
	for n := 0; n < 20000; n++ {
		base := []byte(cases[rng.Intn(len(cases))])
		for k := rng.Intn(3); k >= 0 && len(base) > 0; k-- {
			p := rng.Intn(len(base))
			switch rng.Intn(3) {
			case 0:
				base[p] = alpha[rng.Intn(len(alpha))]
			case 1:
				base = append(base[:p], base[p+1:]...)
			default:
				base = append(base[:p], append([]byte{alpha[rng.Intn(len(alpha))]}, base[p:]...)...)
			}
		}
		cases = append(cases, string(base))
	}
	for _, c := range cases {
		checkDecode(t, []byte(c))
	}
}

// refRequest is protoRequest as encoding/json saw it before the server
// stopped linking encoding/json.
type refRequest struct {
	ID         json.RawMessage `json:"id"`
	Op         string          `json:"op"`
	SQL        string          `json:"sql"`
	SearchMode string          `json:"search_mode"`
}

// checkDecode compares decodeRequest with json.Unmarshal: same success or
// failure, and on success the same fields. (Error texts differ.)
func checkDecode(t *testing.T, line []byte) {
	t.Helper()
	var a protoRequest
	var b refRequest
	errA := decodeRequest(line, &a)
	errB := json.Unmarshal(line, &b)
	if (errA == nil) != (errB == nil) {
		t.Fatalf("%q: decodeRequest error %v, json.Unmarshal error %v", line, errA, errB)
	}
	if errB != nil {
		return
	}
	if string(a.ID) != string(b.ID) || (a.ID == nil) != (b.ID == nil) || a.Op != b.Op || a.SQL != b.SQL || a.SearchMode != b.SearchMode {
		t.Fatalf("%q:\n got  %q %q %q %q\n json %q %q %q %q", line, a.ID, a.Op, a.SQL, a.SearchMode, b.ID, b.Op, b.SQL, b.SearchMode)
	}
}
