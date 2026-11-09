package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func m14WALSize(t *testing.T, e *Engine) int64 {
	t.Helper()
	f, err := os.Stat(filepath.Join(e.dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	return f.Size()
}

func TestM14AutoCheckpointCommits(t *testing.T) {
	e := openTest(t, t.TempDir())
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	for i := uint64(1); i < AutoCheckpointEveryN; i++ {
		execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}
	if n := m14WALSize(t, e); n != 0 {
		t.Fatal("automatic checkpoint did not clear WAL", n)
	}
	execTest(t, s, "INSERT INTO t VALUES (1024)")
	e.Close()
	e = openTest(t, e.dir)
	if n := len(execTest(t, e.Session(), "SELECT * FROM t").Rows); n != 1024 {
		t.Fatal(n)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if m14WALSize(t, e) != 0 {
		t.Fatal("explicit checkpoint")
	}
}

func TestM14AutoCheckpointBytesDeferral(t *testing.T) {
	for _, end := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(end, func(t *testing.T) {
			e := openTest(t, t.TempDir())
			s := e.Session()
			execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, payload TEXT)")
			execTest(t, s, "BEGIN")
			// Another session crosses the byte threshold while this transaction pins state.
			writer := e.Session()
			execTest(t, writer, "INSERT INTO t VALUES (1,'"+strings.Repeat("x", int(AutoCheckpointWALBytes))+"')")
			if m14WALSize(t, e) < AutoCheckpointWALBytes {
				t.Fatal("checkpoint ran during transaction")
			}
			if end == "COMMIT" {
				if s.Execute("COMMIT").Error == "" {
					t.Fatal("expected conflict")
				}
				if m14WALSize(t, e) == 0 {
					t.Fatal("conflicted transaction still pins state")
				}
			}
			execTest(t, s, "ROLLBACK")
			if m14WALSize(t, e) != 0 {
				t.Fatal("deferred checkpoint not run")
			}
			e.Close()
			e = openTest(t, e.dir)
			if len(execTest(t, e.Session(), "SELECT id FROM t").Rows) != 1 {
				t.Fatal("lost commit")
			}
			// A large explicit transaction crosses the threshold only when committed.
			s = e.Session()
			execTest(t, s, "BEGIN")
			execTest(t, s, "INSERT INTO t VALUES (2,'"+strings.Repeat("y", int(AutoCheckpointWALBytes))+"')")
			execTest(t, s, end)
			if m14WALSize(t, e) != 0 {
				t.Fatal("transaction completion checkpoint")
			}
			want := 1
			if end == "COMMIT" {
				want = 2
			}
			e.Close()
			e = openTest(t, e.dir)
			if len(execTest(t, e.Session(), "SELECT id FROM t").Rows) != want {
				t.Fatal("transaction outcome")
			}
		})
	}
}

func TestM14AutoCheckpointFailure(t *testing.T) {
	e := openTest(t, t.TempDir())
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	snap := filepath.Join(e.dir, "snapshot.json")
	if err := os.Mkdir(snap, 0700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = logFile
	defer func() { os.Stderr = old; logFile.Close() }()
	for i := uint64(1); i <= AutoCheckpointEveryN; i++ {
		execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}
	os.Stderr = old
	b, err := os.ReadFile(logFile.Name())
	if err != nil || !strings.Contains(string(b), "automatic checkpoint failed:") {
		t.Fatal("missing diagnostic", err)
	}
	if m14WALSize(t, e) == 0 {
		t.Fatal("failure discarded redo")
	}
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	// Recovery retains pending maintenance; an idle SELECT retries it.
	e.Close()
	e = openTest(t, e.dir)
	s = e.Session()
	if len(execTest(t, s, "SELECT * FROM t").Rows) != int(AutoCheckpointEveryN) {
		t.Fatal("lost committed data")
	}
	if m14WALSize(t, e) != 0 {
		t.Fatal("idle retry did not clear WAL")
	}
}

func TestM14RSSObserve(t *testing.T) {
	for _, budget := range []int64{0, 256} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			m9RSS(t, "M14 before workload")
			e, err := OpenWithOptions(t.TempDir(), OpenOptions{MemoryBudgetBytes: budget})
			if err != nil {
				t.Fatal(err)
			}
			s := e.Session()
			execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
			for batch := 0; batch < 32; batch++ {
				values := make([]string, 32)
				for j := range values {
					id := batch*32 + j
					values[j] = fmt.Sprintf("(%d,[%d,%d])", id, id, -id)
				}
				execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
			}
			execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
			for _, mode := range []string{"exact", "ann"} {
				execTest(t, s, "SET search_mode="+mode)
				r := execTest(t, s, "SELECT id FROM t ORDER BY L2_DISTANCE(v,[0,0]) LIMIT 10")
				if len(r.Rows) != 10 || r.Rows[0][0] != int64(0) {
					t.Fatal(r)
				}
			}
			m9RSS(t, "M14 after 1024 rows + exact/ANN queries")
			t.Logf("M14 cache budget=%d peak decoded row bytes=%d; not a process RSS cap", budget, e.PeakResidentRowBytes())
		})
	}
}
