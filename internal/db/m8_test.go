package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestM8CompactionStress(t *testing.T) {
	start := time.Now()
	dir := t.TempDir()
	const budget = 256
	open := func() *Engine {
		e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: budget, AfterRecoverBeforeRebuild: func(e *Engine) {
			if e.Ready() || e.Session().Execute("SELECT 1").Error != "database not ready" {
				t.Fatal("premature ready")
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT, v VECTOR(2))")
	values := make([]string, 1000)
	for i := range values {
		values[i] = fmt.Sprintf("(%d,'%s',[%d,1])", i, strings.Repeat("x", 80), i)
	}
	execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	for cycle := 0; cycle < 3; cycle++ {
		execTest(t, s, "BEGIN")
		execTest(t, s, "UPDATE t SET label='updated' WHERE id >= 0")
		execTest(t, s, "DELETE FROM t WHERE id < 100")
		execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values[:100], ","))
		execTest(t, s, "COMMIT")
	}
	old := e.pages.path
	before, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Compact(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(e.pages.path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size()*2 >= before.Size() {
		t.Fatalf("no effective compaction: %d -> %d", before.Size(), after.Size())
	}
	if _, err = os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old bytes retained", err)
	}
	check := func(e *Engine) {
		t.Helper()
		s := e.Session()
		if !e.Ready() || len(execTest(t, s, "SELECT * FROM t").Rows) != 1000 {
			t.Fatal("rows/ready")
		}
		if r := execTest(t, s, "SELECT label FROM t WHERE id=500"); fmt.Sprint(r.Rows) != "[[updated]]" {
			t.Fatal(r)
		}
		for _, mode := range []string{"exact", "ann"} {
			execTest(t, s, "SET search_mode="+mode)
			if r := execTest(t, s, "SELECT id FROM t WHERE id >= 100 ORDER BY L2_DISTANCE(v,'[502,1]') LIMIT 3"); fmt.Sprint(r.Rows) != "[[502] [501] [503]]" {
				t.Fatal(r)
			}
		}
		if e.PeakResidentRowBytes() > budget || e.ResidentRowBytes() > budget {
			t.Fatal("budget")
		}
		t.Logf("ResidentRowBytes=%d PeakResidentRowBytes=%d ResidentPages=%d", e.ResidentRowBytes(), e.PeakResidentRowBytes(), e.ResidentPages())
	}
	check(e)
	// Abandon the engine without a graceful checkpoint, with private dirty pages.
	execTest(t, s, "BEGIN")
	execTest(t, s, "DELETE FROM t WHERE id >= 100")
	e = open()
	check(e)
	files, _ := filepath.Glob(filepath.Join(dir, "segments", "*"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	t.Logf("PASS: M8 compaction stress rows=1000 budget=%d segment=%d->%d elapsed=%s; abrupt reopen discards uncommitted delete", budget, before.Size(), after.Size(), time.Since(start))
}

func TestM8BudgetBoundary(t *testing.T) {
	start := time.Now()
	e, err := OpenWithOptions(t.TempDir(), OpenOptions{MemoryBudgetBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT)")
	var values []string
	for i := 0; i < 500; i++ {
		values = append(values, fmt.Sprintf("(%d,'small')", i))
	}
	execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
	result := execTest(t, s, "SELECT * FROM t")
	if len(result.Rows) != 500 || result.Rows[499][0] != int64(499) {
		t.Fatal("result")
	}
	if e.ResidentRowBytes() == 0 || e.PeakResidentRowBytes() > 128 || e.ResidentPages() != 1 {
		t.Fatal("eviction/cache budget")
	}
	big := strings.Repeat("z", 4096)
	execTest(t, s, "INSERT INTO t VALUES (500,'"+big+"')")
	r := execTest(t, s, "SELECT label FROM t WHERE id=500")
	if r.Rows[0][0] != big || e.ResidentRowBytes() != 0 || e.ResidentPages() != 0 {
		t.Fatal("oversized row pinned")
	}
	execTest(t, s, "SELECT * FROM t WHERE id=0")
	if e.ResidentRowBytes() == 0 || e.PeakResidentRowBytes() > 128 {
		t.Fatal("cache not reusable")
	}
	if result.Rows[0][1] != "small" {
		t.Fatal("retained results corrupted")
	}
	t.Logf("PASS: M8 budget boundary exclude results=500 oversized=4096 budget=128 ResidentRowBytes=%d PeakResidentRowBytes=%d ResidentPages=%d elapsed=%s", e.ResidentRowBytes(), e.PeakResidentRowBytes(), e.ResidentPages(), time.Since(start))
}

func TestM8CompactTransactionAndFailure(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), OpenOptions{MemoryBudgetBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT)")
	execTest(t, s, "INSERT INTO t VALUES (1,'old')")
	execTest(t, s, "BEGIN")
	execTest(t, s, "UPDATE t SET label='new' WHERE id=1")
	if e.Compact() == nil {
		t.Fatal("compacted pinned references")
	}
	execTest(t, s, "ROLLBACK")
	// Force a short read: compaction must not publish partial reference changes.
	old := e.pages.path
	refs := append([]pageRef(nil), e.state["t"].refs...)
	if err = os.Truncate(old, 1); err != nil {
		t.Fatal(err)
	}
	if e.Compact() == nil || e.pages.path != old || fmt.Sprint(refs) != fmt.Sprint(e.state["t"].refs) {
		t.Fatal("failed copy published")
	}
	e, err = OpenWithOptions(e.dir, OpenOptions{MemoryBudgetBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	if r := execTest(t, e.Session(), "SELECT label FROM t"); fmt.Sprint(r.Rows) != "[[old]]" {
		t.Fatal(r)
	}
}

func TestM8AutoCompact(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), OpenOptions{MemoryBudgetBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT)")
	execTest(t, s, "INSERT INTO t VALUES (1,'"+strings.Repeat("x", 600000)+"')")
	old := e.pages.path
	execTest(t, s, "BEGIN")
	execTest(t, s, "UPDATE t SET label='"+strings.Repeat("y", 600000)+"' WHERE id=1")
	execTest(t, s, "UPDATE t SET label='small' WHERE id=1")
	execTest(t, s, "COMMIT")
	if e.pages.path == old {
		t.Fatal("automatic threshold not triggered")
	}
	info, err := os.Stat(e.pages.path)
	if err != nil || info.Size() > 100 {
		t.Fatal(info, err)
	}
	if r := execTest(t, s, "SELECT label FROM t"); fmt.Sprint(r.Rows) != "[[small]]" {
		t.Fatal(r)
	}
}
