package db

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestM4TypesRecovery(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(2), v VECTOR(2))")
	execTest(t, s, "BEGIN")
	execTest(t, s, "INSERT INTO t VALUES (9223372036854775807, '你好', [0.1,16777217])")
	execTest(t, s, "INSERT INTO t VALUES (-9223372036854775808, '😀好', [1,0])")
	execTest(t, s, "COMMIT")
	check := func(s *Session) {
		t.Helper()
		rows := execTest(t, s, "SELECT * FROM t").Rows
		if len(rows) != 2 || rows[0][0] != int64(math.MaxInt64) || rows[1][0] != int64(math.MinInt64) {
			t.Fatalf("integer corruption: %#v", rows)
		}
		if !reflect.DeepEqual(rows[0][2], []float32{0.1, 16777216}) {
			t.Fatalf("not float32: %#v", rows[0][2])
		}
		if len(execTest(t, s, "SELECT * FROM t WHERE id = 9223372036854775807 AND name = '你好'").Rows) != 1 {
			t.Fatal("integer equality")
		}
	}
	check(s)
	e.Close()
	e = openTest(t, dir)
	s = e.Session()
	check(s)
	// A JSON snapshot must also retain integer tokens and restore typed vectors.
	raw, err := json.Marshal(e.state)
	if err != nil {
		t.Fatal(err)
	}
	snapshotDir := t.TempDir()
	if err = os.WriteFile(filepath.Join(snapshotDir, "snapshot.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	check(openTest(t, snapshotDir).Session())
	for _, literal := range []string{"9223372036854775808", "-9223372036854775809", "1.5", "1e3", "NaN", "Inf"} {
		if s.Execute(fmt.Sprintf("INSERT INTO t VALUES (%s, '好', [0,0])", literal)).Error == "" {
			t.Fatalf("accepted %s", literal)
		}
	}
	if s.Execute("INSERT INTO t VALUES (3, '你好啊', [0,0])").Error == "" {
		t.Fatal("accepted long VARCHAR")
	}
	for _, v := range []string{"[NaN,0]", "[Inf,0]", "[-Inf,0]", "[1e40,0]", "[null,0]", "[\"NaN\",0]", "[0]", "[0,0,0]"} {
		for _, sql := range []string{"INSERT INTO t VALUES (3, '好', " + v + ")", "SELECT * FROM t ORDER BY L2_DISTANCE(v, " + v + ") LIMIT 1"} {
			if s.Execute(sql).Error == "" {
				t.Fatalf("accepted %s", sql)
			}
		}
	}
	for _, sql := range []string{"CREATE TABLE bad (v VECTOR(0))", "CREATE TABLE bad (v VECTOR)", "CREATE TABLE bad (s VARCHAR(0))", "SELECT * FROM t WHERE v = [1,0]", "UPDATE t SET id=2"} {
		if s.Execute(sql).Error == "" {
			t.Fatalf("accepted %s", sql)
		}
		execTest(t, s, "SELECT 1")
	}
	check(s)
}

func TestM4FilterTiesAndANN(t *testing.T) {
	e := openTest(t, t.TempDir())
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT, v VECTOR(1))")
	for _, sql := range []string{"INSERT INTO t VALUES (0, 'skip', [0])", "INSERT INTO t VALUES (9223372036854775807, 'a AND b', [1])", "INSERT INTO t VALUES (-9223372036854775808, 'a AND b', [-1])", "INSERT INTO t VALUES (2, 'a AND b', [2])"} {
		execTest(t, s, sql)
	}
	query := "SELECT * FROM t WHERE label = 'a AND b' ORDER BY L2_DISTANCE(v, [0])"
	rows := execTest(t, s, query+" LIMIT 1").Rows
	if len(rows) != 1 || rows[0][0] != int64(math.MinInt64) {
		t.Fatalf("filter/tie failure: %v", rows)
	}
	rows = execTest(t, s, query+" DESC LIMIT 3").Rows
	if rows[1][0] != int64(math.MinInt64) {
		t.Fatal("DESC tie must use ascending PK")
	}
	if execTest(t, s, "SELECT search_mode").Rows[0][0] != "exact" {
		t.Fatal("default mode")
	}
	execTest(t, s, "SET search_mode=ann")
	rows = execTest(t, s, query+" LIMIT 3").Rows
	if len(rows) != 2 || rows[0][0] != int64(math.MaxInt64) {
		t.Fatalf("ANN must sample after filtering: %v", rows)
	}
	if execTest(t, s, "SELECT search_mode").Rows[0][0] != "ann" {
		t.Fatal("mode readback")
	}
	if execTest(t, e.Session(), "SELECT search_mode").Rows[0][0] != "exact" {
		t.Fatal("session mode leaked")
	}
	execTest(t, s, "SET search_mode=exact")
	if len(execTest(t, s, query+" LIMIT 3").Rows) != 3 {
		t.Fatal("exact switch")
	}
	e.SetDefaultANN(true)
	if execTest(t, e.Session(), "SELECT search_mode").Rows[0][0] != "ann" {
		t.Fatal("default ANN")
	}
	if s.Execute("SET search_mode=bogus").Error == "" {
		t.Fatal("invalid mode")
	}
	execTest(t, s, "SELECT 1")
}
