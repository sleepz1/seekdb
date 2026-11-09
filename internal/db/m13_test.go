package db

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Real acknowledged commits, then SIGKILL with an active transaction. Both
// recovery passes include index construction and (in bounded mode) compaction.
func TestM13Recovery(t *testing.T) {
	const batches, batchRows = 128, 32
	const rows = batches * batchRows
	if dir := os.Getenv("M13_CRASH_DIR"); dir != "" {
		budget, err := strconv.ParseInt(os.Getenv("M13_BUDGET"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		s := e.Session()
		execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, payload TEXT, v VECTOR(2))")
		execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
		for batch := 0; batch < batches; batch++ {
			values := make([]string, batchRows)
			for j := range values {
				id := batch*batchRows + j
				values[j] = fmt.Sprintf("(%d,'%s',[%d,%d])", id, strings.Repeat("x", 64), id, -id)
			}
			execTest(t, s, "BEGIN")
			execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
			execTest(t, s, "COMMIT")
			if batch == batches/2-1 && os.Getenv("M13_CHECKPOINT") == "true" {
				if err := e.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Exercise mutations that invalidate bucket membership and row offsets.
		execTest(t, s, "UPDATE t SET v = [-1,1] WHERE id = 0")
		execTest(t, s, fmt.Sprintf("DELETE FROM t WHERE id = %d", rows-1))
		execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d,'%s',[%d,%d])", rows-1, strings.Repeat("x", 64), rows-1, 1-rows))
		execTest(t, s, "BEGIN")
		execTest(t, s, "DELETE FROM t")
		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Kill(); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, budget := range []int64{0, 256} {
		for _, checkpoint := range []bool{false, true} {
			t.Run(fmt.Sprintf("budget=%d/checkpoint=%t", budget, checkpoint), func(t *testing.T) {
				dir := t.TempDir()
				cmd := exec.Command(os.Args[0], "-test.run=^TestM13Recovery$")
				cmd.Env = append(os.Environ(), "M13_CRASH_DIR="+dir, fmt.Sprintf("M13_BUDGET=%d", budget), fmt.Sprintf("M13_CHECKPOINT=%t", checkpoint))
				out, err := cmd.CombinedOutput()
				dead, ok := err.(*exec.ExitError)
				if !ok || dead.ProcessState.ExitCode() != -1 {
					t.Fatalf("child did not die by signal: %v %s", err, out)
				}
				info, err := os.Stat(filepath.Join(dir, "wal.log"))
				if err != nil {
					t.Fatal(err)
				}
				for pass := 0; pass < 2; pass++ {
					start := time.Now()
					e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: budget, AfterRecoverBeforeRebuild: func(e *Engine) {
						if e.Session().Execute("SELECT 1").Error == "" {
							t.Fatal("ready before rebuild")
						}
						if !checkpoint && e.state["t"].Indexes[0].buckets != nil {
							t.Fatal("replay rebuilt derived buckets before final pass")
						}
					}})
					elapsed := time.Since(start)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("M13 recovery: rows=%d wal_bytes=%d pass=%d duration=%s rows/sec=%.0f (Open through readiness; observational)", rows, info.Size(), pass, elapsed, float64(rows)/elapsed.Seconds())
					table := e.state["t"]
					if table.rowCount() != rows || len(table.Indexes) != 1 {
						t.Fatal("lost rows/index metadata")
					}
					for id := 0; id < rows; id++ {
						row := table.rowAt(id)
						x, y := float32(id), float32(-id)
						if id == 0 {
							x, y = -1, 1
						}
						v := row[2].([]float32)
						if row[0] != int64(id) || row[1] != strings.Repeat("x", 64) || len(v) != 2 || v[0] != x || v[1] != y {
							t.Fatalf("row %d: %v", id, row)
						}
					}
					// Every recovered bucket must reference the current row exactly once.
					seen := make(map[int]bool)
					for key, offsets := range table.Indexes[0].buckets {
						for _, n := range offsets {
							if n < 0 || n >= rows || seen[n] || bucket(table.rowAt(n)[2].([]float32)) != key {
								t.Fatal("invalid recovered bucket", key, n)
							}
							seen[n] = true
						}
					}
					if len(seen) != rows {
						t.Fatal("incomplete recovered index")
					}
					s := e.Session()
					for _, mode := range []string{"exact", "ann"} {
						execTest(t, s, "SET search_mode="+mode)
						r := execTest(t, s, "SELECT id FROM t ORDER BY L2_DISTANCE(v,[-1,1]) LIMIT 1")
						if len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
							t.Fatal(mode, r)
						}
					}
					if budget > 0 && e.PeakResidentRowBytes() > budget {
						t.Fatal("cache budget exceeded")
					}
					e.Close()
				}
			})
		}
	}
}
