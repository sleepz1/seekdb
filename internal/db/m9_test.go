package db

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestM9WALChecksum(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	execTest(t, s, "INSERT INTO t VALUES (1)")
	p := filepath.Join(dir, "wal.log")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte(`"checksum":`)) != 2 {
		t.Fatal("missing checksums")
	}
	if r := execTest(t, openTest(t, dir).Session(), "SELECT id FROM t"); fmt.Sprint(r.Rows) != "[[1]]" {
		t.Fatal(r)
	}
	// A bad middle record must prevent later records from being replayed.
	bad := bytes.Replace(data, []byte("VALUES (1)"), []byte("VALUES (9)"), 1)
	bad = append(bad, data[bytes.IndexByte(data, '\n')+1:]...)
	if err = os.WriteFile(p, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal(err)
	}
}

func TestM9WALCorruptTail(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	execTest(t, s, "INSERT INTO t VALUES (1)")
	prefix, err := os.ReadFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	execTest(t, s, "INSERT INTO t VALUES (2)")
	full, err := os.ReadFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"payload", "checksum", "missing"} {
		t.Run(kind, func(t *testing.T) {
			target := t.TempDir()
			p := filepath.Join(target, "wal.log")
			tail := append([]byte(nil), full[len(prefix):]...)
			switch kind {
			case "payload":
				tail = bytes.Replace(tail, []byte("VALUES (2)"), []byte("VALUES (9)"), 1)
			case "checksum":
				i := bytes.Index(tail, []byte(`"checksum":`)) + len(`"checksum":`)
				if tail[i] == '1' {
					tail[i] = '2'
				} else {
					tail[i] = '1'
				}
			case "missing":
				i := bytes.Index(tail, []byte(`,"checksum":`))
				tail = append(tail[:i], '}', '\n')
			}
			bad := append(append([]byte(nil), prefix...), tail...)
			if err := os.WriteFile(p, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(target); err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatal(err)
			}
			unchanged, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(unchanged, bad) {
				t.Fatal("corrupt WAL modified", err)
			}
			// Explicit operator repair at the reported record offset recovers the prefix.
			if err := os.Truncate(p, int64(len(prefix))); err != nil {
				t.Fatal(err)
			}
			if r := execTest(t, openTest(t, target).Session(), "SELECT id FROM t"); fmt.Sprint(r.Rows) != "[[1]]" {
				t.Fatal(r)
			}
		})
	}
	for _, n := range []int{1, (len(full) - len(prefix)) / 2, len(full) - len(prefix) - 1} {
		target := t.TempDir()
		if err := os.WriteFile(filepath.Join(target, "wal.log"), full[:len(prefix)+n], 0600); err != nil {
			t.Fatal(err)
		}
		if r := execTest(t, openTest(t, target).Session(), "SELECT id FROM t"); fmt.Sprint(r.Rows) != "[[1]]" {
			t.Fatal(r)
		}
	}
}

func TestM9WALHardCrash(t *testing.T) {
	if dir := os.Getenv("M9_CRASH_DIR"); dir != "" {
		e := openTest(t, dir)
		s := e.Session()
		execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
		execTest(t, s, "INSERT INTO t VALUES (1)")
		if os.Getenv("M9_CRASH_PHASE") == "after" {
			execTest(t, s, "BEGIN")
			execTest(t, s, "INSERT INTO t VALUES (2)")
			execTest(t, s, "COMMIT")
		} else {
			// Fail the real streaming writer after it flushes a partial COMMIT record,
			// before its checksum, newline and fsync. No timing-dependent sleep.
			err := e.persistBatch(func(emit func(string) error) error {
				if err := emit("INSERT INTO t VALUES (2)" + strings.Repeat(" ", 16384)); err != nil {
					return err
				}
				return errors.New("injected interrupted commit")
			})
			if err == nil {
				t.Fatal("missing failure")
			}
			data, err := os.ReadFile(filepath.Join(dir, "wal.log"))
			if err != nil || data[len(data)-1] == '\n' {
				t.Fatal("no partial write", err)
			}
		}
		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Kill(); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestM9WALHardCrash$")
			cmd.Env = append(os.Environ(), "M9_CRASH_DIR="+dir, "M9_CRASH_PHASE="+phase)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("child survived")
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ProcessState.ExitCode() != -1 {
				t.Fatalf("child did not die by signal: %v %s", err, out)
			}
			want := "[[1]]"
			if phase == "after" {
				want = "[[1] [2]]"
			}
			for i := 0; i < 2; i++ {
				if r := execTest(t, openTest(t, dir).Session(), "SELECT id FROM t"); fmt.Sprint(r.Rows) != want {
					t.Fatal(r)
				}
			}
		})
	}
}

func m9RSS(t *testing.T, phase string) {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Log("VmRSS unavailable; observation skipped")
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			t.Logf("%s %s (observational only)", phase, line)
			return
		}
	}
}
func TestM9ANNRecallCoarse(t *testing.T) {
	m9RSS(t, "before")
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
	// Four sign-separated clusters, deterministic small offsets around (+/-10,+/-10).
	var values []string
	for c := 0; c < 4; c++ {
		for i := 0; i < 40; i++ {
			x, y := 10+float64(i)/100, 10+float64((i*17)%41)/100
			if c&1 != 0 {
				x = -x
			}
			if c&2 != 0 {
				y = -y
			}
			values = append(values, fmt.Sprintf("(%d,[%.2f,%.2f])", c*40+i, x, y))
		}
	}
	execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	total := 0
	for _, q := range []string{"[10.15,10.17]", "[-10.15,10.17]", "[10.15,-10.17]", "[-10.15,-10.17]"} {
		sql := "SELECT id FROM t ORDER BY L2_DISTANCE(v,'" + q + "') LIMIT 10"
		execTest(t, s, "SET search_mode=exact")
		exact := execTest(t, s, sql)
		execTest(t, s, "SET search_mode=ann")
		ann := execTest(t, s, sql)
		if len(exact.Rows) != 10 || len(ann.Rows) == 0 {
			t.Fatal("empty/invalid topK")
		}
		ids := map[int64]bool{}
		for _, r := range exact.Rows {
			ids[r[0].(int64)] = true
		}
		hits := 0
		for _, r := range ann.Rows {
			if ids[r[0].(int64)] {
				hits++
			}
		}
		recall := float64(hits) / 10
		total += hits
		t.Logf("query=%s recall@10=%.2f", q, recall)
		if recall < 0.1 {
			t.Fatal("ANN degenerated")
		}
	}
	t.Logf("mean recall@10=%.2f rows=160 queries=4 floor=0.10 (synthetic coarse regression, not contest metrics)", float64(total)/40)
	m9RSS(t, "after")
}
