package db

import (
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func execTest(t *testing.T, s *Session, sql string) Result {
	t.Helper()
	r := s.Execute(sql)
	if r.Error != "" {
		t.Fatalf("%s: %s", sql, r.Error)
	}
	return r
}
func TestWALTransactions(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
	execTest(t, s, "BEGIN")
	execTest(t, s, "COMMIT") // no LSN gap
	execTest(t, s, "START TRANSACTION")
	execTest(t, s, "INSERT INTO t VALUES (1, [1,0])")
	if len(execTest(t, e.Session(), "SELECT * FROM t").Rows) != 0 {
		t.Fatal("dirty read")
	}
	execTest(t, s, "COMMIT")
	execTest(t, s, "BEGIN")
	execTest(t, s, "INSERT INTO t VALUES (2, [0,1])")
	execTest(t, s, "ROLLBACK")
	execTest(t, s, "BEGIN")
	execTest(t, s, "INSERT INTO t VALUES (3, [0,1])")
	e.Close()
	recovered := openTest(t, dir)
	rows := execTest(t, recovered.Session(), "SELECT * FROM t ORDER BY DISTANCE(v, [1,0])").Rows
	if len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("recovered: %v", rows)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("unexpected snapshot")
	}
}
func TestWALTornTailAndRepeatRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"lsn":2,"statements":["INSERT INTO t VALUES (99)"]}`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.e.Close()
	s = openTest(t, dir).Session()
	execTest(t, s, "INSERT INTO t VALUES (1)")
	for i := 0; i < 2; i++ {
		s.e.Close()
		s = openTest(t, dir).Session()
		rows := execTest(t, s, "SELECT * FROM t").Rows
		if len(rows) != 1 || rows[0][0] != int64(1) {
			t.Fatal(rows)
		}
	}
}
func TestWALRejectsCorruption(t *testing.T) {
	for _, data := range []string{"bad\n", "{\"lsn\":2,\"statements\":[\"CREATE TABLE t (id BIGINT PRIMARY KEY)\"]}\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "wal.log"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Fatal("accepted corrupt WAL")
		}
	}
}
func TestSnapshotBaseConflictAndDeletes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(`{"t":{"Columns":[{"Name":"id","Type":"INT"}],"Rows":[[1]]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	e := openTest(t, dir)
	a, b := e.Session(), e.Session()
	execTest(t, a, "BEGIN")
	execTest(t, a, "INSERT INTO t VALUES (2)")
	execTest(t, b, "DELETE FROM t")
	if a.Execute("COMMIT").Error == "" {
		t.Fatal("missing conflict")
	}
	execTest(t, a, "ROLLBACK")
	e.Close()
	s := openTest(t, dir).Session()
	if len(execTest(t, s, "SELECT * FROM t").Rows) != 0 {
		t.Fatal("delete lost")
	}
	execTest(t, s, "DROP TABLE t")
	s.e.Close()
	if openTest(t, dir).Session().Execute("SELECT * FROM t").Error == "" {
		t.Fatal("drop lost")
	}
}

func TestTransactionRecordAtomicRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir).Session()
	execTest(t, s, "BEGIN")
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	execTest(t, s, "INSERT INTO t VALUES (1)")
	if s.Execute("INSERT INTO t VALUES ('bad')").Error == "" {
		t.Fatal("invalid statement accepted")
	}
	execTest(t, s, "INSERT INTO t VALUES (2)")
	if len(execTest(t, s, "SELECT * FROM t").Rows) != 2 {
		t.Fatal("read-your-writes failed")
	}
	execTest(t, s, "COMMIT")
	record, err := os.ReadFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	// Crash at different offsets within one multi-statement commit must expose
	// either the entire transaction or none of it, never a partially built table.
	for _, n := range []int{0, len(record) / 2, len(record) - 1, len(record)} {
		target := t.TempDir()
		if err := os.WriteFile(filepath.Join(target, "wal.log"), record[:n], 0600); err != nil {
			t.Fatal(err)
		}
		r := openTest(t, target).Session().Execute("SELECT * FROM t")
		if n < len(record) {
			if r.Error == "" {
				t.Fatalf("partial transaction at %d: %+v", n, r)
			}
		} else if r.Error != "" || len(r.Rows) != 2 {
			t.Fatalf("full transaction: %+v", r)
		}
	}
}
