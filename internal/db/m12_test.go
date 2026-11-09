package db

import (
	"fmt"
	"testing"
)

// An occupied nearby shell must not hide eligible rows in distant shells.
func TestM12ANNFilteredExpansion(t *testing.T) {
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, category BIGINT, v VECTOR(8))")
	for i := 0; i < 24; i++ {
		category := 0
		if i >= 12 {
			category = 1
		}
		vector := "[1,1,1,1,1,1,1,1]"
		if i >= 13 {
			vector = "[-1,-1,-1,-1,-1,-1,-1,-1]"
		}
		execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d,%d,%s)", i, category, vector))
	}
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	execTest(t, s, "SET search_mode=ann")
	for _, limit := range []int{0, 10, 30} {
		sql := fmt.Sprintf("SELECT id FROM t WHERE category = 1 ORDER BY L2_DISTANCE(v,[1,1,1,1,1,1,1,1]) LIMIT %d", limit)
		r := execTest(t, s, sql)
		want := limit
		if want > 12 {
			want = 12
		}
		if len(r.Rows) != want {
			t.Fatalf("limit=%d: %v", limit, r)
		}
		for i, row := range r.Rows {
			if row[0] != int64(12+i) {
				t.Fatalf("predicate/tie order: %v", r)
			}
		}
	}
}
