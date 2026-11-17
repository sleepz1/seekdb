package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// TestHelperSplitMatchesSequential runs TopK queries with the scan split
// between the query goroutine and the pre-woken helper (exchanging their
// k-th bounds) and compares every result with the single-threaded scan,
// on data with many exact ties and on continuous data, with and without
// equality filters (posting lists) and with free slots.
func TestHelperSplitMatchesSequential(t *testing.T) {
	for _, ties := range []bool{true, false} {
		d := newTDB(t, Options{})
		d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, cat VARCHAR(4), emb VECTOR(32))")
		rng := rand.New(rand.NewSource(5))
		vec := func() string {
			v := make([]float32, 32)
			for i := range v {
				if ties {
					v[i] = float32(rng.Intn(3))
				} else {
					v[i] = float32(rng.Intn(2000001)-1000000) / 1e6
				}
			}
			return FormatVector(v)
		}
		for i := 0; i < 6000; i += 500 {
			var vals []string
			for j := i; j < i+500; j++ {
				vals = append(vals, fmt.Sprintf("(%d,'c%d','%s')", (j*7919)%100003, j%3, vec()))
			}
			d.exec("INSERT INTO v VALUES " + strings.Join(vals, ","))
		}
		d.exec("DELETE FROM v WHERE id < 1500")

		oldW, oldMin, oldH := Workers, helperMinWork, helper
		Workers, helperMinWork, helper = 2, 1, nil
		startHelper()
		h := helper
		defer func() { Workers, helperMinWork, helper = oldW, oldMin, oldH }()
		used := 0
		for q := 0; q < 60; q++ {
			where := ""
			if q%2 == 1 {
				where = fmt.Sprintf(" WHERE cat = 'c%d'", q%3)
			}
			sql := fmt.Sprintf("SELECT id, L2_DISTANCE(emb, '%s') AS dist FROM v%s ORDER BY dist LIMIT %d", vec(), where, 1+q%25)
			helper = nil
			seq := rowsString(d.exec(sql))
			helper = h
			deadline := time.Now().Add(100 * time.Millisecond)
			for Prewake(); h.state.Load() != helperSpin && time.Now().Before(deadline); Prewake() {
			}
			if h.state.Load() == helperSpin {
				used++
			}
			par := rowsString(d.exec(sql))
			if seq != par {
				t.Fatalf("ties=%v split differs for %s:\n seq %s\n par %s", ties, sql, seq, par)
			}
		}
		// A helper whose thread never runs: every split is taken back by
		// the query goroutine after its own half.
		ghost := &scanHelper{wake: make(chan struct{}, 1)}
		ghost.state.Store(helperSpin)
		for q := 0; q < 20; q++ {
			where := ""
			if q%2 == 1 {
				where = fmt.Sprintf(" WHERE cat = 'c%d'", q%3)
			}
			sql := fmt.Sprintf("SELECT id, L2_DISTANCE(emb, '%s') AS dist FROM v%s ORDER BY dist LIMIT %d", vec(), where, 1+q%25)
			helper = nil
			seq := rowsString(d.exec(sql))
			helper = ghost
			par := rowsString(d.exec(sql))
			if seq != par {
				t.Fatalf("ties=%v taken-back split differs for %s:\n seq %s\n par %s", ties, sql, seq, par)
			}
			if s := ghost.state.Load(); s != helperSpin {
				t.Fatalf("state %d after a taken-back split", s)
			}
		}
		helper = h
		if used < 30 {
			t.Fatalf("helper was awake for only %d of 60 queries", used)
		}
		// Idle: the helper parks again after its window.
		time.Sleep(20 * time.Millisecond)
		if s := h.state.Load(); s != helperIdle {
			t.Fatalf("helper state %d after idling", s)
		}
	}
}
