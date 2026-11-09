package db

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestM15Lock(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	execTest(t, s, "BEGIN")
	execTest(t, s, "INSERT INTO t VALUES (1)")
	if other, err := OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: 128}); err == nil {
		other.Close()
		t.Fatal("second open accepted")
	} else if !strings.Contains(err.Error(), "cannot lock data directory") {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Execute("COMMIT").Error == "" || e.Ready() || e.Checkpoint() == nil || e.Compact() == nil {
		t.Fatal("closed engine still usable")
	}
	next := openTest(t, dir)
	if len(execTest(t, next.Session(), "SELECT * FROM t").Rows) != 0 {
		t.Fatal("uncommitted transaction recovered")
	}
	// Re-closing the old engine must not unlock its successor.
	e.Close()
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("successor lost lock")
	}
	if _, err := os.Stat(filepath.Join(dir, "LOCK")); err != nil {
		t.Fatal(err)
	}
}

func TestM15LockFailedOpen(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		dir := t.TempDir()
		if !panicHook {
			os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("invalid"), 0600)
		}
		opts := OpenOptions{}
		if panicHook {
			opts.AfterRecoverBeforeRebuild = func(*Engine) { panic(storageError{fmt.Errorf("injected storage error")}) }
		}
		if e, err := OpenWithOptions(dir, opts); err == nil {
			e.Close()
			t.Fatal("expected open failure")
		}
		if !panicHook {
			if err := os.Remove(filepath.Join(dir, "snapshot.json")); err != nil {
				t.Fatal(err)
			}
		}
		openTest(t, dir)
	}
}

func TestM15LockProcess(t *testing.T) {
	if dir := os.Getenv("M15_LOCK_DIR"); dir != "" {
		e, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		execTest(t, e.Session(), "CREATE TABLE t (id BIGINT PRIMARY KEY)")
		fmt.Println("LOCK_HELD")
		// Parent keeps stdin open until it kills us.
		bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestM15LockProcess$")
	cmd.Env = append(os.Environ(), "M15_LOCK_DIR="+dir)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "LOCK_HELD\n" {
		t.Fatalf("child readiness: %q %v", line, err)
	}
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("concurrent process accepted")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("child survived SIGKILL")
	}
	e := openTest(t, dir)
	execTest(t, e.Session(), "SELECT * FROM t")
}
