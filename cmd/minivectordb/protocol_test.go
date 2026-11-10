package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestOfficialProtocolHandshakeAndSQL(t *testing.T) {
	dir := t.TempDir()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"-data", dir}, inR, outW, &stderr)
		_ = outW.Close()
	}()

	br := bufio.NewReader(outR)
	ready := readLineTimeout(t, br, 5*time.Second)
	if ready != `{"type":"ready"}` {
		t.Fatalf("ready before any request: got %q, stderr=%s", ready, stderr.String())
	}

	// CREATE
	writeLine(t, inW, `{"id":1,"op":"execute","sql":"CREATE TABLE docs (id BIGINT PRIMARY KEY, content VARCHAR(16), embedding VECTOR(2));","search_mode":"exact"}`)
	got := mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	assertOK(t, got, 1)
	// INSERT
	writeLine(t, inW, `{"id":2,"op":"execute","sql":"INSERT INTO docs VALUES (1, '你好', '[3,4]');","search_mode":"exact"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	assertOK(t, got, 2)
	if intFrom(t, got["affected_rows"]) != 1 {
		t.Fatalf("affected_rows: %v", got["affected_rows"])
	}
	// SELECT including BIGINT as number and vector as array
	writeLine(t, inW, `{"id":3,"op":"execute","sql":"SELECT id, content, embedding FROM docs ORDER BY id;","search_mode":"exact"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	assertOK(t, got, 3)
	cols, _ := got["columns"].([]any)
	if len(cols) != 3 || cols[0] != "id" || cols[1] != "content" || cols[2] != "embedding" {
		t.Fatalf("columns: %v", got["columns"])
	}
	rows, _ := got["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", got["rows"])
	}
	row, _ := rows[0].([]any)
	if len(row) != 3 {
		t.Fatalf("row: %v", row)
	}
	if _, ok := row[0].(json.Number); !ok {
		// encoding/json default is float64 unless UseNumber; we parse with UseNumber below
	}
	if n := numberAsInt(t, row[0]); n != 1 {
		t.Fatalf("id want 1 got %v (%T)", row[0], row[0])
	}
	if row[1] != "你好" {
		t.Fatalf("content: %v", row[1])
	}
	vec, ok := row[2].([]any)
	if !ok || len(vec) != 2 {
		t.Fatalf("embedding should be JSON array, got %T %v", row[2], row[2])
	}

	// empty result still has columns
	writeLine(t, inW, `{"id":4,"op":"execute","sql":"SELECT id FROM docs WHERE id = 99;","search_mode":"exact"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	assertOK(t, got, 4)
	rows, _ = got["rows"].([]any)
	if rows == nil || len(rows) != 0 {
		t.Fatalf("empty rows should be [], got %v", got["rows"])
	}

	// expected error keeps the process alive
	writeLine(t, inW, `{"id":5,"op":"execute","sql":"INSERT INTO docs VALUES (1, 'dup', '[0,0]');","search_mode":"exact"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	if got["ok"] != false {
		t.Fatalf("expected ok:false, got %v", got)
	}
	if _, ok := got["id"].(json.Number); !ok && !isInt(got["id"]) {
		t.Fatalf("id type should stay a number, got %T %v", got["id"], got["id"])
	}
	if numberAsInt(t, got["id"]) != 5 {
		t.Fatalf("id: %v", got["id"])
	}
	errObj, _ := got["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	if code == "" {
		t.Fatalf("missing error.code: %v", got)
	}

	// large BIGINT stays a JSON integer
	writeLine(t, inW, `{"id":6,"op":"execute","sql":"CREATE TABLE nums (id BIGINT PRIMARY KEY, value BIGINT);","search_mode":"exact"}`)
	_ = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	writeLine(t, inW, `{"id":7,"op":"execute","sql":"INSERT INTO nums VALUES (9007199254740993, -9007199254740993);","search_mode":"exact"}`)
	_ = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	writeLine(t, inW, `{"id":8,"op":"execute","sql":"SELECT id, value FROM nums;","search_mode":"exact"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	rows, _ = got["rows"].([]any)
	row, _ = rows[0].([]any)
	if numberAsInt(t, row[0]) != 9007199254740993 || numberAsInt(t, row[1]) != -9007199254740993 {
		t.Fatalf("bigint lost precision: %v", row)
	}

	writeLine(t, inW, `{"id":9,"op":"shutdown"}`)
	got = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
	assertOK(t, got, 9)
	_ = inW.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d, stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after shutdown")
	}

	// leftover stdout should be empty
	rest, _ := io.ReadAll(br)
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("extra stdout after shutdown: %q", rest)
	}
}

func TestOfficialProtocolReadyBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	inR, inW := io.Pipe()
	defer inW.Close()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	go func() {
		_ = run([]string{"--data-dir", dir}, inR, outW, &stderr)
		_ = outW.Close()
	}()
	br := bufio.NewReader(outR)
	// Do not write any request first.
	line := readLineTimeout(t, br, 5*time.Second)
	if line != `{"type":"ready"}` {
		t.Fatalf("got %q", line)
	}
	writeLine(t, inW, `{"id":1,"op":"shutdown"}`)
	_ = mustJSON(t, readLineTimeout(t, br, 5*time.Second))
}

func TestOfficialProtocolRestartReady(t *testing.T) {
	dir := t.TempDir()
	runOnce := func() {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		var stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- run([]string{"-data", dir}, inR, outW, &stderr)
			_ = outW.Close()
		}()
		br := bufio.NewReader(outR)
		if got := readLineTimeout(t, br, 5*time.Second); got != `{"type":"ready"}` {
			t.Fatalf("ready: %q stderr=%s", got, stderr.String())
		}
		writeLine(t, inW, `{"id":1,"op":"execute","sql":"CREATE TABLE IF NOT EXISTS t (id BIGINT PRIMARY KEY);","search_mode":"exact"}`)
		assertOK(t, mustJSON(t, readLineTimeout(t, br, 5*time.Second)), 1)
		writeLine(t, inW, `{"id":2,"op":"shutdown"}`)
		assertOK(t, mustJSON(t, readLineTimeout(t, br, 5*time.Second)), 2)
		_ = inW.Close()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit %d stderr=%s", code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no exit")
		}
	}
	runOnce()
	runOnce() // recovery restart must also emit ready
}

func TestLegacyTSVStillWorks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	var stdout, stderr bytes.Buffer
	in := bytes.NewBufferString("CREATE TABLE t (id BIGINT PRIMARY KEY, c VARCHAR(8));\nINSERT INTO t VALUES (1, 'a');\nSELECT id, c FROM t;\n")
	code := run([]string{"--data-dir", dir, "--format", "tsv"}, in, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !bytes.Contains(stdout.Bytes(), []byte("OK 1")) {
		t.Fatalf("expected TSV OK framing, got %q", got)
	}
	if bytes.Contains(stdout.Bytes(), []byte(`"type":"ready"`)) {
		t.Fatalf("legacy TSV must not emit official ready: %q", got)
	}
}

func writeLine(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s+"\n"); err != nil {
		t.Fatal(err)
	}
}

func readLineTimeout(t *testing.T, br *bufio.Reader, d time.Duration) string {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := br.ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read line: %v", r.err)
		}
		return string(bytes.TrimRight([]byte(r.s), "\r\n"))
	case <-time.After(d):
		t.Fatal("timed out waiting for a stdout line")
		return ""
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("json %q: %v", s, err)
	}
	return m
}

func assertOK(t *testing.T, got map[string]any, id int64) {
	t.Helper()
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", got)
	}
	if numberAsInt(t, got["id"]) != id {
		t.Fatalf("id: got %v want %d", got["id"], id)
	}
}

func isInt(v any) bool {
	switch v.(type) {
	case int, int64, json.Number, float64:
		return true
	}
	return false
}

func intFrom(t *testing.T, v any) int64 {
	t.Helper()
	return numberAsInt(t, v)
}

func numberAsInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatalf("not int: %v %v", v, err)
		}
		return i
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("want number, got %T %v", v, v)
		return 0
	}
}
