package main

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// jsonreq.go (a request parser without encoding/json) is not used by the
// server in this revision; this test keeps it equivalent to json.Unmarshal.

// Random JSON documents, valid and mutated, decoded by parseRequest and by json.Unmarshal.
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
		checkParse(t, b)
	}
	// nesting depth limit of encoding/json
	for _, d := range []int{9999, 10000, 10001} {
		doc := `{"id":` + strings.Repeat("[", d) + strings.Repeat("]", d) + `,"op":"x"}`
		checkParse(t, []byte(doc))
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

// checkParse compares parseRequest with json.Unmarshal: same success or
// failure, and on success the same fields. (Error texts differ.)
func checkParse(t *testing.T, line []byte) {
	t.Helper()
	var a, b protoRequest
	errA := parseRequest(line, &a)
	errB := json.Unmarshal(line, &b)
	if (errA == nil) != (errB == nil) {
		t.Fatalf("%q: parseRequest error %v, json.Unmarshal error %v", line, errA, errB)
	}
	if errB != nil {
		return
	}
	if string(a.ID) != string(b.ID) || (a.ID == nil) != (b.ID == nil) || a.Op != b.Op || a.SQL != b.SQL || a.SearchMode != b.SearchMode {
		t.Fatalf("%q:\n got  %q %q %q %q\n json %q %q %q %q", line, a.ID, a.Op, a.SQL, a.SearchMode, b.ID, b.Op, b.SQL, b.SearchMode)
	}
}
