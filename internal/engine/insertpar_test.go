package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestParallelInsertErrorOrder: large multi-row INSERTs convert vectors in
// parallel, but the reported error must be the one sequential processing
// hits first, and nothing may be applied.
func TestParallelInsertErrorOrder(t *testing.T) {
	old := Workers
	defer func() { Workers = old }()
	mk := func(bad map[int]string) string {
		var r []string
		for i := 0; i < 64; i++ {
			c, v := "'ok'", "'[1,2,3]'"
			switch bad[i] {
			case "dim":
				v = "'[1,2]'"
			case "nan":
				v = "'[1,NaN,3]'"
			case "long":
				c = "'toolongvalue'"
			case "null":
				v = "NULL"
			case "dup":
				r = append(r, fmt.Sprintf("(%d,%s,%s)", 0, c, v))
				continue
			}
			r = append(r, fmt.Sprintf("(%d,%s,%s)", i, c, v))
		}
		return "INSERT INTO v VALUES " + strings.Join(r, ",")
	}
	cases := []map[int]string{
		{10: "dim", 3: "long"}, {3: "dim", 10: "long"}, {5: "nan", 6: "dim"}, {7: "null", 2: "nan"},
		{1: "dup", 30: "dim"}, {40: "dim"}, {63: "nan"}, {},
	}
	for _, bad := range cases {
		var outs []string
		for _, w := range []int{1, 2, 3} {
			Workers = w
			d := newTDB(t, Options{})
			d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, c VARCHAR(4), e VECTOR(3))")
			_, err := d.s.ExecSQL(mk(bad))
			cnt := d.exec("SELECT id FROM v")
			outs = append(outs, fmt.Sprintf("%v rows=%d", err, len(cnt.Rows)))
		}
		if outs[0] != outs[1] || outs[0] != outs[2] {
			t.Fatalf("%v: sequential %q, parallel %q / %q", bad, outs[0], outs[1], outs[2])
		}
	}
}
