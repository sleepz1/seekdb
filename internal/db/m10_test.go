package db

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestM10WALCheckpoint(t *testing.T) {
	for _, budget := range []int64{0, 128} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			dir := t.TempDir()
			open := func() *Engine {
				e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: budget})
				if err != nil {
					t.Fatal(err)
				}
				return e
			}
			e := open()
			s := e.Session()
			execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
			execTest(t, s, "INSERT INTO t VALUES (1,[1,2]),(2,[2,3])")
			execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
			execTest(t, s, "DELETE FROM t WHERE id = 2")
			execTest(t, s, "BEGIN")
			execTest(t, s, "INSERT INTO t VALUES (3,[3,4])")
			if err := e.Checkpoint(); err == nil {
				t.Fatal("active transaction accepted")
			}
			execTest(t, s, "ROLLBACK")
			p := filepath.Join(dir, "wal.log")
			old, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p)
			if err != nil || info.Size() != 0 {
				t.Fatal("WAL not empty", err)
			}
			// Emulate interruption after snapshot publication, before WAL truncation.
			if err = os.WriteFile(p, old, 0600); err != nil {
				t.Fatal(err)
			}
			e = open()
			s = e.Session()
			if r := execTest(t, s, "SELECT id FROM t"); fmt.Sprint(r.Rows) != "[[1]]" {
				t.Fatal(r)
			}
			execTest(t, s, "INSERT INTO t VALUES (4,[4,5])")
			e = open()
			s = e.Session()
			if r := execTest(t, s, "SELECT id FROM t ORDER BY L2_DISTANCE(v,[4,5]) LIMIT 1"); fmt.Sprint(r.Rows) != "[[4]]" {
				t.Fatal(r)
			}
			if len(e.state["t"].Indexes) != 1 {
				t.Fatal("index metadata lost")
			}
			if err = e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			execTest(t, s, "INSERT INTO t VALUES (5,[5,6])")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			bad := bytes.Replace(data, []byte("VALUES (5"), []byte("VALUES (9"), 1)
			if err = os.WriteFile(p, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Open(dir); err == nil {
				t.Fatal("post-checkpoint checksum corruption accepted")
			}
		})
	}
}

func TestM10WALBoundedGrowth(t *testing.T) {
	if dir := os.Getenv("M10_CRASH_DIR"); dir != "" {
		e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 128})
		if err != nil {
			t.Fatal(err)
		}
		s := e.Session()
		execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
		for cycle := 0; cycle < 3; cycle++ {
			for i := 0; i < 200; i++ {
				execTest(t, s, "BEGIN")
				execTest(t, s, fmt.Sprintf("INSERT INTO t VALUES (%d)", cycle*200+i))
				execTest(t, s, "COMMIT")
			}
			p := filepath.Join(dir, "wal.log")
			before, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(p)
			if err != nil || after.Size() != 0 || before.Size() == 0 {
				t.Fatal("growth bound", err)
			}
			fmt.Printf("WAL cycle=%d %d->%d bytes\n", cycle, before.Size(), after.Size())
		}
		execTest(t, s, "INSERT INTO t VALUES (600)")
		execTest(t, s, "BEGIN")
		execTest(t, s, "INSERT INTO t VALUES (999)")
		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Kill(); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestM10WALBoundedGrowth$")
	cmd.Env = append(os.Environ(), "M10_CRASH_DIR="+dir)
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ProcessState.ExitCode() != -1 {
		t.Fatalf("child did not die by signal: %v %s", err, out)
	}
	t.Logf("%s", out)
	for i := 0; i < 2; i++ {
		e, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 128})
		if err != nil {
			t.Fatal(err)
		}
		r := execTest(t, e.Session(), "SELECT id FROM t ORDER BY id")
		if len(r.Rows) != 601 {
			t.Fatal(len(r.Rows))
		}
		for j, row := range r.Rows {
			if row[0] != int64(j) {
				t.Fatal(j, row)
			}
		}
		if e.PeakResidentRowBytes() > 128 {
			t.Fatal("cache exceeded")
		}
	}
}

func TestM10CheckpointFailure(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	execTest(t, s, "INSERT INTO t VALUES (1)")
	// A failed rename must leave redo intact and must not poison healthy writes.
	snap := filepath.Join(dir, "snapshot.json")
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snap, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "wal.log")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err == nil {
		t.Fatal("rename failure missing")
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("redo changed", err)
	}
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	execTest(t, s, "INSERT INTO t VALUES (2)")
	e = openTest(t, dir)
	if len(execTest(t, e.Session(), "SELECT * FROM t").Rows) != 2 {
		t.Fatal("lost commit")
	}
	e.walErr = fmt.Errorf("injected failed WAL sync")
	if err := e.Checkpoint(); err == nil {
		t.Fatal("poisoned writer checkpointed")
	}
}
