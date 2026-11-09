package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestM7BeyondMemory(t *testing.T) {
	dir := t.TempDir()
	const budget = 512
	open := func() *Engine {
		e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT, v VECTOR(2))")
	execTest(t, s, "BEGIN")
	for i := 0; i < 160; i++ {
		execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d,'%s',[%d,1])", i, strings.Repeat("x", 100), i))
	}
	if len(s.statements) != 0 || s.spool == "" || s.txWrites != 160 {
		t.Fatal("transaction payloads not spooled")
	}
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	execTest(t, s, "COMMIT")
	check := func(e *Engine) {
		t.Helper()
		s := e.Session()
		if n := len(execTest(t, s, "SELECT id FROM t").Rows); n != 160 {
			t.Fatal(n)
		}
		r := execTest(t, s, "SELECT id FROM t WHERE id >= 100 ORDER BY L2_DISTANCE(v,'[102,1]') LIMIT 3")
		if fmt.Sprint(r.Rows) != "[[102] [101] [103]]" {
			t.Fatal(r)
		}
		execTest(t, s, "SET search_mode=ann")
		if r = execTest(t, s, "SELECT id FROM t WHERE id >= 100 ORDER BY L2_DISTANCE(v,'[102,1]') LIMIT 1"); r.Rows[0][0] != int64(102) {
			t.Fatal(r)
		}
		if e.ResidentRowBytes() == 0 || e.PeakResidentRowBytes() == 0 || e.ResidentRowBytes() > budget || e.PeakResidentRowBytes() > budget || e.ResidentPages() >= 160 {
			t.Fatal("budget ignored")
		}
		t.Logf("rows=160 budget=%d resident=%d peak=%d pages=%d", budget, e.ResidentRowBytes(), e.PeakResidentRowBytes(), e.ResidentPages())
		if len(e.state["t"].Rows) != 0 || len(e.state["t"].refs) != 160 {
			t.Fatal("payloads retained")
		}
	}
	check(e)
	// Close releases ownership without checkpointing: only acknowledged WAL commits may recover.
	e.Close()
	e = open()
	check(e)
	s = e.Session()
	execTest(t, s, "BEGIN")
	execTest(t, s, "UPDATE t SET label='changed',v='[-1,-1]' WHERE id=102")
	execTest(t, s, "DELETE FROM t WHERE id < 100")
	if len(execTest(t, s, "SELECT id FROM t").Rows) != 60 {
		t.Fatal("transaction scan")
	}
	execTest(t, s, "ROLLBACK")
	check(e)
	execTest(t, s, "BEGIN")
	execTest(t, s, "INSERT INTO t VALUES (160,'ok',[1,1])")
	if s.Execute("INSERT INTO t VALUES (161,'bad',[1,1]),(160,'duplicate',[1,1])").Error == "" {
		t.Fatal("duplicate accepted")
	}
	execTest(t, s, "UPDATE t SET label='changed' WHERE id=102")
	execTest(t, s, "DELETE FROM t WHERE id < 100")
	execTest(t, s, "COMMIT")
	e.Close()
	e = open()
	s = e.Session()
	if len(execTest(t, s, "SELECT id FROM t").Rows) != 61 {
		t.Fatal("committed mutations lost")
	}
	if len(execTest(t, s, "SELECT id FROM t WHERE label='changed'").Rows) != 1 {
		t.Fatal("update lost")
	}
	execTest(t, s, "DELETE FROM t")
	execTest(t, s, "DROP TABLE t")
	e.Close()
	if open().Session().Execute("SELECT * FROM t").Error == "" {
		t.Fatal("drop lost")
	}
	t.Log("PASS: M7 beyond-memory bounded payloads, cold TopK, WAL reopen, mutations and rollback")
}

func TestM7ReadyGate(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(1))")
	execTest(t, s, "INSERT INTO t VALUES (1,[1]),(2,[-1])")
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	s.e.Close()
	observed := false
	e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 128, AfterRecoverBeforeRebuild: func(e *Engine) {
		observed = true
		if e.Ready() {
			t.Fatal("premature readiness")
		}
		if r := e.Session().Execute("SELECT 1"); r.Error != "database not ready" {
			t.Fatal(r)
		}
		// Erase replay-built buckets: final startup rebuild must restore them.
		e.state["t"].Indexes[0].buckets = nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !observed || !e.Ready() {
		t.Fatal("gate not completed")
	}
	s = e.Session()
	execTest(t, s, "SET search_mode=ann")
	if r := execTest(t, s, "SELECT id FROM t ORDER BY L2_DISTANCE(v,'[1]') LIMIT 1"); fmt.Sprint(r.Rows) != "[[1]]" {
		t.Fatal(r)
	}
	t.Log("PASS: M7 ready gate rejects Execute before rebuild and serves rebuilt index afterwards")
}

func TestM7StreamingSnapshot(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, `{"t":{"Rows":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			fmt.Fprint(f, ",")
		}
		fmt.Fprintf(f, `[%d,"%s"]`, i, strings.Repeat("z", 300))
	}
	fmt.Fprint(f, `],"Columns":[{"Name":"id","Type":"BIGINT","Primary":true},{"Name":"text","Type":"TEXT"}]}}`)
	f.Close()
	e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(execTest(t, e.Session(), "SELECT id FROM t").Rows) != 200 || len(e.state["t"].Rows) != 0 || e.PeakResidentRowBytes() > 512 {
		t.Fatal("snapshot not bounded")
	}
}

func TestM7PageFailure(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
	execTest(t, s, "INSERT INTO t VALUES (1,[1,1])")
	if err = os.Remove(e.pages.path); err != nil {
		t.Fatal(err)
	}
	if s.Execute("SELECT * FROM t").Error == "" {
		t.Fatal("page error hidden")
	}
	if s.Execute("INSERT INTO t VALUES (2,[2,2])").Error == "" {
		t.Fatal("failed page write acknowledged")
	}
	e.Close()
	e, err = OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if r := execTest(t, e.Session(), "SELECT id FROM t"); fmt.Sprint(r.Rows) != "[[1]]" {
		t.Fatal(r)
	}
}
