package main

import (
	"bufio"
	"bytes"
	"minivectordb/internal/db"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Documents and locks the interim stdin/stdout framing used by tip#1.
// Official companion-driver bytes are unpublished — do NOT treat PASS here as
// "official framing confirmed" (see FORMAL-MYSQL-GAP-MATRIX.md).

func TestFormalFramingInterimTSVAndFlush(t *testing.T) {
	if activeResultCodec != codecInterimTSV {
		t.Fatalf("default codec must stay interimTSV, got %v", activeResultCodec)
	}
	dir := t.TempDir()
	e, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var out bytes.Buffer
	input := strings.Join([]string{
		"CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(8), content VARCHAR(16), embedding VECTOR(2));",
		"INSERT INTO docs VALUES (1, '甲', '一', '[0,0]'), (2, '乙', '二', '[3,4]');",
		"SELECT id, L2_DISTANCE(embedding, '[0,0]') AS distance FROM docs ORDER BY distance LIMIT 2;",
		"SELECT id FROM docs WHERE id = 99;",
		"INSERT INTO docs VALUES (1, 'x', 'y', '[0,0]');",
		"BEGIN;",
		"INSERT INTO docs VALUES (3, '丙', '三', '[1,0]');",
	}, "\n")
	if err := repl(strings.NewReader(input), &out, e.Session()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	wantParts := []string{
		"OK 0\n",
		"OK 2\n",
		"id\tdistance\n1\t0\n2\t5\nOK 2\n",
		"id\nOK 0\n",
		"ERROR ",
		"OK 0\n",
		"OK 1\n",
	}
	pos := 0
	for _, p := range wantParts {
		i := strings.Index(got[pos:], p)
		if i < 0 {
			t.Fatalf("missing framing fragment %q in:\n%s", p, got)
		}
		pos += i + len(p)
	}
	e.Close()
	e2, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	r := e2.Session().Execute("SELECT id FROM docs ORDER BY id")
	if r.Error != "" || len(r.Rows) != 2 {
		t.Fatalf("EOF must roll back open txn; got %+v", r)
	}
}

func TestFormalFramingMultiStatementSemicolon(t *testing.T) {
	e, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var out bytes.Buffer
	in := "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(1));INSERT INTO t VALUES (1,[1]);SELECT id FROM t;\n"
	if err := repl(strings.NewReader(in), &out, e.Session()); err != nil {
		t.Fatal(err)
	}
	if out.String() != "OK 0\nOK 1\nid\n1\nOK 1\n" {
		t.Fatal(out.String())
	}
}

func TestFormalFramingReadyOnStderrOnly(t *testing.T) {
	if os.Getenv("FORMAL_FRAMING_READY_CHILD") == "1" {
		dir := os.Getenv("FORMAL_FRAMING_READY_DIR")
		e, err := db.Open(dir)
		if err != nil {
			os.Stderr.WriteString("open fail\n")
			os.Exit(2)
		}
		defer e.Close()
		reportReady()
		_ = repl(strings.NewReader("SELECT 1;\n"), os.Stdout, e.Session())
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormalFramingReadyOnStderrOnly$")
	cmd.Env = append(os.Environ(), "FORMAL_FRAMING_READY_CHILD=1", "FORMAL_FRAMING_READY_DIR="+dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "minivectordb: ready\n") {
		t.Fatalf("ready missing on stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "ready") {
		t.Fatalf("ready leaked to stdout: %q", stdout.String())
	}
	if stdout.String() != "1\n1\nOK 1\n" {
		t.Fatalf("stdout framing: %q", stdout.String())
	}
}

func TestFormalFramingCodecHookDefaultsUnchanged(t *testing.T) {
	prev := activeResultCodec
	defer func() { activeResultCodec = prev }()
	activeResultCodec = codecInterimTSV
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := writeResult(w, db.Result{Affected: 3}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "OK 3\n" {
		t.Fatal(buf.String())
	}
	if err := writeResult(w, db.Result{Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	_ = w.Flush()
	if !strings.HasSuffix(buf.String(), "ERROR boom\n") {
		t.Fatal(buf.String())
	}
}
