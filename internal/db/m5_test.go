package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestM5Constraints(t *testing.T) {
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VECTOR(2))")
	execTest(t, s, "INSERT INTO t VALUES (1,[0,0])")
	for _, sql := range []string{
		"INSERT INTO t VALUES (1,[1,1])",
		"CREATE TABLE bad (id BIGINT PRIMARY KEY, a VECTOR(1), b VECTOR(1))",
		"CREATE TABLE bad (id BIGINT)",
		"CREATE TABLE bad (id INT PRIMARY KEY)",
		"CREATE TABLE bad (id BIGINT PRIMARY KEY, x BIGINT PRIMARY KEY)",
	} {
		if s.Execute(sql).Error == "" {
			t.Fatal(sql)
		}
	}
	if len(execTest(t, s, "SELECT * FROM t").Rows) != 1 {
		t.Fatal("partial write")
	}
}

func TestM5Mutations(t *testing.T) {
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label VARCHAR(2), v VECTOR(2))")
	r := execTest(t, s, "INSERT INTO t VALUES (1,'你好','[1, 0]'),(2,'ab',[0,1])")
	if r.Affected != 2 {
		t.Fatal(r)
	}
	for _, sql := range []string{
		"INSERT INTO t VALUES (3,'ok',[0,0]),(1,'ok',[0,0])",
		"INSERT INTO t VALUES (3,'ok',[0,0]),(4,'bad',[0,0])",
		"UPDATE t SET id=3 WHERE id=1",
		"UPDATE t SET label='ok',v='[0]' WHERE id=1",
	} {
		if s.Execute(sql).Error == "" {
			t.Fatal(sql)
		}
	}
	execTest(t, s, "BEGIN")
	if execTest(t, s, "UPDATE t SET v='[0.8,0.2]',label='好' WHERE id=1").Affected != 1 {
		t.Fatal("update count")
	}
	if execTest(t, s, "DELETE FROM t WHERE id=2").Affected != 1 {
		t.Fatal("delete count")
	}
	execTest(t, s, "ROLLBACK")
	rows := execTest(t, s, "SELECT * FROM t").Rows
	if len(rows) != 2 || rows[0][1] != "你好" || rows[0][2].([]float32)[0] != 1 {
		t.Fatal(rows)
	}
}

func TestM5Select(t *testing.T) {
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(32), content VARCHAR(1024), embedding VECTOR(4))")
	execTest(t, s, "INSERT INTO docs VALUES (1,'database','事务与恢复','[1,0,0,0]'),(2,'database','向量索引','[0,1,0,0]'),(3,'travel','旅行攻略','[0,0,1,0]')")
	r := execTest(t, s, "SELECT id, content, L2_DISTANCE(embedding, '[0.9, 0.1, 0, 0]') AS distance FROM docs WHERE category = 'database' ORDER BY distance LIMIT 2")
	if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][1] != "向量索引" || r.Columns[2] != "distance" {
		t.Fatal(r)
	}
	for _, op := range []string{"<", "<=", ">", ">="} {
		r := execTest(t, s, "SELECT id FROM docs WHERE id "+op+" 2 AND category >= 'database' ORDER BY id DESC")
		want := map[string]int{"<": 1, "<=": 2, ">": 1, ">=": 2}[op]
		if len(r.Rows) != want {
			t.Fatal(op, r)
		}
	}
	execTest(t, s, "BEGIN")
	if execTest(t, s, "UPDATE docs SET embedding='[0.8,0.2,0,0]' WHERE id <= 1 AND category = 'database'").Affected != 1 {
		t.Fatal("update")
	}
	if execTest(t, s, "DELETE FROM docs WHERE id >= 2 AND id < 3").Affected != 1 {
		t.Fatal("delete")
	}
	execTest(t, s, "ROLLBACK")
	if len(execTest(t, s, "SELECT * FROM docs").Rows) != 3 {
		t.Fatal("rollback")
	}
	r = execTest(t, s, "SELECT content FROM docs ORDER BY category DESC LIMIT 1")
	if r.Rows[0][0] != "旅行攻略" {
		t.Fatal(r)
	}
	execTest(t, s, "SELECT id,L2_DISTANCE(embedding,'[0,0,0,0]') d FROM docs ORDER BY d LIMIT 2")
	execTest(t, s, "SELECT L2_DISTANCE(embedding,'[0,0,0,0]') FROM docs ORDER BY L2_DISTANCE(embedding,'[0,0,0,0]') DESC LIMIT 2")
	for _, sql := range []string{"SELECT * FROM docs WHERE embedding > '[0,0,0,0]'", "SELECT id FROM docs ORDER BY missing", "SELECT id FROM docs LIMIT -1"} {
		if s.Execute(sql).Error == "" {
			t.Fatal(sql)
		}
	}
}

func TestM5Index(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT, v VECTOR(1))")
	execTest(t, s, "INSERT INTO t VALUES (1,'a',[-1]),(2,'b',[1]),(3,'a',[-2])")
	execTest(t, s, "CREATE VECTOR INDEX idx ON t(v)")
	for _, sql := range []string{"CREATE VECTOR INDEX idx ON t(v)", "CREATE VECTOR INDEX other ON missing(v)", "CREATE VECTOR INDEX other ON t(id)"} {
		if s.Execute(sql).Error == "" {
			t.Fatal(sql)
		}
	}
	query := "SELECT id FROM t ORDER BY L2_DISTANCE(v,'[1]') LIMIT 3"
	check := func(s *Session) {
		execTest(t, s, "SET search_mode=ann")
		r := execTest(t, s, query)
		if len(r.Rows) != 3 || r.Rows[0][0] != int64(2) {
			t.Fatalf("index path: %+v", r)
		}
		execTest(t, s, "SET search_mode=exact")
		r = execTest(t, s, query)
		if len(r.Rows) != 3 || r.Rows[0][0] != int64(2) {
			t.Fatal(r)
		}
	}
	check(s)
	check(openTest(t, dir).Session())
	execTest(t, s, "SET search_mode=ann")
	execTest(t, s, "BEGIN")
	execTest(t, s, "UPDATE t SET v='[-3]' WHERE id=2")
	execTest(t, s, "INSERT INTO t VALUES (4,'b',[1])")
	r := execTest(t, s, query)
	if len(r.Rows) != 3 || r.Rows[0][0] != int64(4) {
		t.Fatal(r)
	}
	execTest(t, s, "DELETE FROM t WHERE id=4")
	if len(execTest(t, s, query).Rows) != 3 {
		t.Fatal("empty bucket fallback")
	}
	execTest(t, s, "ROLLBACK")
	check(s)
	execTest(t, s, "SET search_mode=ann")
	r = execTest(t, s, "SELECT id FROM t WHERE label='a' ORDER BY L2_DISTANCE(v,'[1]') LIMIT 3")
	if len(r.Rows) != 2 {
		t.Fatal("filter before candidates", r)
	}
}

func TestM5IndexSnapshotAndCommittedMutations(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir)
	s := e.Session()
	execTest(t, s, "CREATE TABLE t (other BIGINT, id BIGINT PRIMARY KEY, label TEXT, v VECTOR(1))")
	execTest(t, s, "INSERT INTO t VALUES (0,2,'a',[1]),(9,1,'b',[-1])")
	execTest(t, s, "CREATE VECTOR INDEX idx ON t(v)")
	r := execTest(t, s, "SELECT id FROM t ORDER BY L2_DISTANCE(v,'[0]') DESC")
	if r.Rows[0][0] != int64(1) {
		t.Fatal("must tie by explicit PK", r)
	}
	execTest(t, s, "BEGIN")
	execTest(t, s, "UPDATE t SET v='[2]',label='a WHERE b' WHERE id=1")
	execTest(t, s, "DELETE FROM t WHERE id=2")
	execTest(t, s, "COMMIT")
	check := func(s *Session) {
		execTest(t, s, "SET search_mode=ann")
		r := execTest(t, s, "SELECT id,label FROM t ORDER BY L2_DISTANCE(v,'[2]')")
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != "a WHERE b" {
			t.Fatal(r)
		}
		if s.Execute("INSERT INTO t VALUES (3,1,'x',[3])").Error == "" {
			t.Fatal("lost PK")
		}
		if s.Execute("CREATE VECTOR INDEX idx ON t(v)").Error == "" {
			t.Fatal("lost metadata")
		}
	}
	check(openTest(t, dir).Session())
	raw, err := json.Marshal(e.state)
	if err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err = os.WriteFile(filepath.Join(snap, "snapshot.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	check(openTest(t, snap).Session())
}
