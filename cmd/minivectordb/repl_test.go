package main

import (
	"bytes"
	"minivectordb/internal/db"
	"strings"
	"testing"
)

func TestREPLFramingAndRecovery(t *testing.T) {
	dir := t.TempDir()
	e, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	input := "CREATE TABLE t (\nid BIGINT PRIMARY KEY, label VARCHAR, v VECTOR(2)\n);\nBEGIN; INSERT INTO t VALUES (1, 'a;''b\tc', [1,0]); COMMIT;\nINSERT INTO t VALUES (1.5, 'bad', [0,1]);\nSELECT * FROM t ORDER BY L2_DISTANCE(v, [1,0]);\nBEGIN; INSERT INTO t VALUES (2, 'lost', [0,1]);"
	if err := repl(strings.NewReader(input), &out, e.Session()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ERROR type_mismatch BIGINT requires an integer in int64 range\nid\tlabel\tv\n1\ta;'b\\tc\t[1,0]\nOK 1\n") {
		t.Fatal(out.String())
	}
	e.Close()
	e, err = db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := repl(strings.NewReader("SELECT * FROM t;SELECT 1"), &out, e.Session()); err != nil {
		t.Fatal(err)
	}
	if out.String() != "id\tlabel\tv\n1\ta;'b\\tc\t[1,0]\nOK 1\n1\n1\nOK 1\n" {
		t.Fatal(out.String())
	}
}
