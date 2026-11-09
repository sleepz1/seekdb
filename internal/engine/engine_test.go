package engine

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type tdb struct {
	t    *testing.T
	dir  string
	opts Options
	e    *Engine
	s    *Session
}

func newTDB(t *testing.T, opts Options) *tdb {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	d := &tdb{t: t, dir: opts.Dir, opts: opts}
	d.open()
	return d
}

func (d *tdb) open() {
	d.t.Helper()
	e, err := Open(d.opts)
	if err != nil {
		d.t.Fatalf("open: %v", err)
	}
	d.e, d.s = e, e.NewSession("exact")
}

func (d *tdb) restart() {
	d.s.Close()
	d.e.Close()
	d.open()
}

// crash drops the engine without any shutdown work, like SIGKILL.
func (d *tdb) crash() {
	d.e.log.f.Close()
	d.open()
}

func (d *tdb) exec(sql string) *Result {
	d.t.Helper()
	r, err := d.s.ExecSQL(sql)
	if err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return r
}

func (d *tdb) fail(sql string, kind ErrKind) {
	d.t.Helper()
	_, err := d.s.ExecSQL(sql)
	if err == nil {
		d.t.Fatalf("%s: expected error", sql)
	}
	if se := AsSQLError(err); se.Kind != kind {
		d.t.Fatalf("%s: error kind %d (%v), want %d", sql, se.Kind, err, kind)
	}
}

func rowsString(r *Result) string {
	var sb strings.Builder
	for _, row := range r.Rows {
		for j, v := range row {
			if j > 0 {
				sb.WriteByte(',')
			}
			switch v.K {
			case KInt:
				fmt.Fprint(&sb, v.I)
			case KStr:
				sb.WriteString(v.S)
			case KFloat:
				fmt.Fprintf(&sb, "%.4f", v.F)
			case KVec:
				sb.WriteString(FormatVector(v.V))
			}
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

func (d *tdb) query(sql, want string) {
	d.t.Helper()
	if got := rowsString(d.exec(sql)); got != want {
		d.t.Fatalf("%s:\n got  %s\n want %s", sql, got, want)
	}
}

const ddl = "CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(8), content VARCHAR(16), embedding VECTOR(2))"

func TestPersistenceAndRestart(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec(ddl)
	d.exec("INSERT INTO docs VALUES (3,'b','three','[0,2]'),(1,'a','one','[0,0]'),(2,'a','two','[1,0]')")
	d.exec("UPDATE docs SET content = 'uno' WHERE id = 1")
	d.exec("DELETE FROM docs WHERE id = 3")
	d.exec("CREATE VECTOR INDEX idx ON docs(embedding)")
	d.restart()
	d.query("SELECT * FROM docs ORDER BY id", "1,a,uno,[0,0];2,a,two,[1,0];")
	d.fail("CREATE VECTOR INDEX idx ON docs(embedding)", ErrIndexExists)
	d.crash()
	d.query("SELECT id FROM docs ORDER BY id", "1;2;")
}

func TestTransactionSemantics(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec(ddl)
	d.exec("INSERT INTO docs VALUES (1,'a','one','[0,0]'),(2,'a','two','[1,0]')")

	d.exec("BEGIN")
	d.exec("INSERT INTO docs VALUES (3,'b','three','[0,0.5]')")
	d.exec("UPDATE docs SET embedding = '[5,5]' WHERE id = 1")
	d.exec("DELETE FROM docs WHERE id = 2")
	// read-your-writes, both scalar and vector
	d.query("SELECT id FROM docs ORDER BY id", "1;3;")
	d.query("SELECT id, L2_DISTANCE(embedding,'[0,0]') AS d FROM docs ORDER BY d LIMIT 1", "3,0.5000;")
	// a failing statement leaves earlier work intact and adds nothing
	d.fail("INSERT INTO docs VALUES (4,'c','x','[0,0]'),(3,'c','dup','[0,0]')", ErrDupKey)
	d.query("SELECT id FROM docs ORDER BY id", "1;3;")
	d.exec("ROLLBACK")
	d.query("SELECT id, L2_DISTANCE(embedding,'[0,0]') AS d FROM docs ORDER BY d", "1,0.0000;2,1.0000;")

	d.exec("BEGIN")
	d.exec("DELETE FROM docs WHERE id = 1")
	d.exec("INSERT INTO docs VALUES (1,'z','again','[9,9]')") // delete then re-insert same pk
	d.exec("COMMIT")
	d.restart()
	d.query("SELECT id, category FROM docs ORDER BY id", "1,z;2,a;")

	// uncommitted work never survives a crash or shutdown
	d.exec("BEGIN")
	d.exec("INSERT INTO docs VALUES (7,'q','lost','[1,1]')")
	d.crash()
	d.query("SELECT id FROM docs ORDER BY id", "1;2;")
	d.exec("BEGIN")
	d.exec("INSERT INTO docs VALUES (8,'q','lost','[1,1]')")
	d.restart()
	d.query("SELECT id FROM docs ORDER BY id", "1;2;")

	// insert+delete of the same key inside one transaction is a no-op
	d.exec("BEGIN")
	d.exec("INSERT INTO docs VALUES (9,'q','tmp','[1,1]')")
	d.exec("DELETE FROM docs WHERE id = 9")
	d.exec("COMMIT")
	d.query("SELECT id FROM docs ORDER BY id", "1;2;")
}

func TestTornTailIsTruncated(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec(ddl)
	d.exec("INSERT INTO docs VALUES (1,'a','one','[0,0]')")
	size := d.e.log.size
	d.exec("INSERT INTO docs VALUES (2,'a','two','[1,0]')")
	full := d.e.log.size
	d.e.log.f.Close()
	path := filepath.Join(d.dir, logName)
	// Simulate a kill in the middle of writing the second record.
	for cut := size + 1; cut < full; cut += 7 {
		if err := os.Truncate(path, cut); err != nil {
			t.Fatal(err)
		}
		d.open()
		d.query("SELECT id FROM docs ORDER BY id", "1;")
		if d.e.log.size != size {
			t.Fatalf("cut %d: log size %d, want %d", cut, d.e.log.size, size)
		}
		d.e.log.f.Close()
	}
	// Garbage after a valid record is also discarded, and recovery is repeatable.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{9, 0, 0, 0, 1, 2, 3, 4, 5})
	f.Close()
	d.open()
	d.crash()
	d.query("SELECT id FROM docs ORDER BY id", "1;")
	d.exec("INSERT INTO docs VALUES (3,'a','x','[1,1]')")
	d.restart()
	d.query("SELECT id FROM docs ORDER BY id", "1;3;")
}

func TestValidationErrors(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec(ddl)
	d.fail("CREATE TABLE docs (id BIGINT PRIMARY KEY)", ErrTableExists)
	d.fail("CREATE TABLE t2 (id BIGINT, v VARCHAR(3))", ErrNoPK)
	d.fail("CREATE TABLE t2 (id VARCHAR(3) PRIMARY KEY)", ErrBadPKType)
	d.fail("CREATE TABLE t2 (id BIGINT PRIMARY KEY, a VECTOR(2), b VECTOR(2))", ErrMultiVector)
	d.fail("INSERT INTO docs VALUES (1,'abcdefghi','x','[0,0]')", ErrDataTooLong)
	d.exec("INSERT INTO docs VALUES (1,'甲乙丙丁戊己庚辛','x','[0,0]')") // 8 runes fits VARCHAR(8)
	d.fail("INSERT INTO docs VALUES (2,'a','x','[0,0,0]')", ErrVectorDim)
	d.fail("INSERT INTO docs VALUES (2,'a','x','[NaN,0]')", ErrVectorValue)
	d.fail("INSERT INTO docs VALUES (2,'a','x','[Infinity,0]')", ErrVectorValue)
	d.fail("INSERT INTO docs VALUES (2,'a','x','[1e39,0]')", ErrVectorValue)
	d.fail("INSERT INTO docs VALUES (2,'a','x')", ErrValueCount)
	d.fail("INSERT INTO docs VALUES (2,'a','x',NULL)", ErrNullValue)
	d.fail("INSERT INTO docs VALUES (99999999999999999999,'a','x','[0,0]')", ErrOutOfRange)
	d.fail("UPDATE docs SET id = 5 WHERE id = 1", ErrPKUpdate)
	d.fail("UPDATE docs SET embedding = '[1]' WHERE id = 1", ErrVectorDim)
	d.fail("SELECT id FROM docs ORDER BY L2_DISTANCE(embedding, '[1,2,3]') LIMIT 1", ErrVectorDim)
	d.fail("SELECT id FROM nope", ErrNoTable)
	d.fail("SELECT id FROM docs GROUP BY id", ErrUnsupported)
	d.fail("SELECT COUNT(*) FROM docs", ErrParse)
	d.fail("SELECT id FROM docs WHERE id IS NULL", ErrUnsupported)
	d.fail("SELEC id FROM docs", ErrParse)
	d.fail("CREATE VECTOR INDEX i2 ON docs(content)", ErrBadIndexColumn)
	d.fail("BEGIN; BEGIN", ErrParse)
	d.exec("BEGIN")
	d.fail("BEGIN", ErrTxnState)
	d.exec("ROLLBACK")
	// none of the failures above changed data
	d.query("SELECT id, category FROM docs", "1,甲乙丙丁戊己庚辛;")
	d.exec("INSERT INTO docs VALUES (-9223372036854775808,'min','x','[-3.4e38,3.4e38]')")
	d.query("SELECT id FROM docs ORDER BY id", "-9223372036854775808;1;")
}

func TestOrderingAndComparisons(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec(ddl)
	d.exec("INSERT INTO docs VALUES (5,'b','e','[1,0]'),(4,'a','d','[0,1]'),(3,'b','c','[-1,0]'),(2,'a','b','[0,-1]'),(1,'c','a','[2,0]')")
	// equal distances (1.0) tie-break on primary key ascending, also with DESC
	d.query("SELECT id FROM docs ORDER BY L2_DISTANCE(embedding,'[0,0]') LIMIT 3", "2;3;4;")
	d.query("SELECT id FROM docs ORDER BY L2_DISTANCE(embedding,'[0,0]') DESC LIMIT 2", "1;2;")
	d.query("SELECT id FROM docs ORDER BY category DESC, id DESC", "1;5;3;4;2;")
	d.query("SELECT id FROM docs ORDER BY category LIMIT 2 OFFSET 1", "4;3;")
	d.query("SELECT content AS c FROM docs WHERE category = 'b' AND id >= 4", "e;")
	d.query("SELECT id FROM docs WHERE category != 'a' AND id < 5 ORDER BY id", "1;3;")
	// filter is applied before TopK: the global nearest rows are 'a' and 'b'
	d.query("SELECT id, L2_DISTANCE(embedding,'[2,0]') AS d FROM docs WHERE category = 'a' ORDER BY d LIMIT 5", "2,2.2361;4,2.2361;")
	d.query("SELECT id FROM docs WHERE category = 'zzz' ORDER BY L2_DISTANCE(embedding,'[0,0]') LIMIT 3", "")
	d.query("SELECT id FROM docs ORDER BY 1 DESC LIMIT 1", "5;")
	d.query("SELECT L2_DISTANCE('[3,4]','[0,0]') AS d", "5.0000;")
}

// TestRandomizedAgainstReference compares filtered TopK, updates and deletes
// with a brute-force model, both cached and disk-only, across restarts.
func TestRandomizedAgainstReference(t *testing.T) {
	for _, budget := range []int64{1 << 30, 1} {
		t.Run(fmt.Sprintf("cache=%d", budget), func(t *testing.T) {
			d := newTDB(t, Options{CacheBudget: budget, CompactMinBytes: 4096})
			d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, cat VARCHAR(4), emb VECTOR(8))")
			d.exec("CREATE VECTOR INDEX vi ON v(emb)")
			rng := rand.New(rand.NewSource(42))
			model := map[int64]struct {
				cat string
				v   []float32
			}{}
			vec := func() []float32 {
				v := make([]float32, 8)
				for i := range v {
					v[i] = float32(rng.Intn(5)) // small integers create many exact ties
				}
				return v
			}
			for round := 0; round < 40; round++ {
				var vals []string
				for k := 0; k < 25; k++ {
					id := int64(rng.Intn(400))
					if _, ok := model[id]; ok {
						continue
					}
					dup := false
					for _, s := range vals {
						if strings.HasPrefix(s, fmt.Sprintf("(%d,", id)) {
							dup = true
						}
					}
					if dup {
						continue
					}
					v := vec()
					c := fmt.Sprintf("c%d", rng.Intn(3))
					vals = append(vals, fmt.Sprintf("(%d,'%s','%s')", id, c, FormatVector(v)))
					model[id] = struct {
						cat string
						v   []float32
					}{c, v}
				}
				if len(vals) > 0 {
					d.exec("INSERT INTO v VALUES " + strings.Join(vals, ","))
				}
				for k := 0; k < 5; k++ {
					id := int64(rng.Intn(400))
					v := vec()
					r := d.exec(fmt.Sprintf("UPDATE v SET emb = '%s' WHERE id = %d", FormatVector(v), id))
					if m, ok := model[id]; ok {
						m.v = v
						model[id] = m
						if r.Affected != 1 {
							t.Fatalf("update affected %d", r.Affected)
						}
					} else if r.Affected != 0 {
						t.Fatalf("update of missing row affected %d", r.Affected)
					}
				}
				del := int64(rng.Intn(400))
				d.exec(fmt.Sprintf("DELETE FROM v WHERE id < %d AND id > %d", del+3, del))
				for id := range model {
					if id > del && id < del+3 {
						delete(model, id)
					}
				}
				if round%7 == 3 {
					d.restart()
				}
				if round%11 == 5 {
					d.crash()
				}
				for _, mode := range []string{"exact", "ann"} {
					d.s.Mode = mode
					q := vec()
					for _, filter := range []string{"", "c1"} {
						type hit struct {
							id int64
							d  float64
						}
						var hits []hit
						for id, m := range model {
							if filter != "" && m.cat != filter {
								continue
							}
							hits = append(hits, hit{id, math.Sqrt(l2sq(m.v, q))})
						}
						sort.Slice(hits, func(i, j int) bool {
							if hits[i].d != hits[j].d {
								return hits[i].d < hits[j].d
							}
							return hits[i].id < hits[j].id
						})
						if len(hits) > 10 {
							hits = hits[:10]
						}
						var want strings.Builder
						for _, h := range hits {
							fmt.Fprintf(&want, "%d,%.4f;", h.id, h.d)
						}
						where := ""
						if filter != "" {
							where = " WHERE cat = '" + filter + "'"
						}
						d.query(fmt.Sprintf("SELECT id, L2_DISTANCE(emb, '%s') AS dist FROM v%s ORDER BY dist LIMIT 10", FormatVector(q), where), want.String())
					}
				}
			}
			// The log was compacted along the way and is still consistent.
			d.restart()
			r := d.exec("SELECT id FROM v")
			if len(r.Rows) != len(model) {
				t.Fatalf("row count %d, want %d", len(r.Rows), len(model))
			}
		})
	}
}

// TestParallelMatchesSequential checks that the multi-worker scan returns
// exactly the sequential result, including primary-key tie-breaks.
func TestParallelMatchesSequential(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, cat VARCHAR(4), emb VECTOR(16))")
	rng := rand.New(rand.NewSource(3))
	vec := func() string {
		v := make([]float32, 16)
		for i := range v {
			v[i] = float32(rng.Intn(3)) // tiny value range: lots of exact ties
		}
		return FormatVector(v)
	}
	for i := 0; i < 9000; i += 500 {
		var vals []string
		for j := i; j < i+500; j++ {
			vals = append(vals, fmt.Sprintf("(%d,'c%d','%s')", (j*7919)%100003, j%4, vec()))
		}
		d.exec("INSERT INTO v VALUES " + strings.Join(vals, ","))
	}
	d.exec("DELETE FROM v WHERE id < 2000") // leave free slots in the array
	old := Workers
	defer func() { Workers = old }()
	for q := 0; q < 40; q++ {
		where := ""
		if q%2 == 1 {
			where = fmt.Sprintf(" WHERE cat = 'c%d' AND id > 5000", q%4)
		}
		sql := fmt.Sprintf("SELECT id, L2_DISTANCE(emb, '%s') AS dist FROM v%s ORDER BY dist LIMIT %d", vec(), where, 1+q%25)
		Workers = 1
		seq := rowsString(d.exec(sql))
		Workers = 4
		par := rowsString(d.exec(sql))
		if seq != par {
			t.Fatalf("parallel differs for %s:\n seq %s\n par %s", sql, seq, par)
		}
	}
}

func TestCompactionKeepsDataAndShrinksLog(t *testing.T) {
	d := newTDB(t, Options{CompactMinBytes: 1 << 40})
	d.exec("CREATE TABLE t (id BIGINT PRIMARY KEY, s VARCHAR(100))")
	for i := 0; i < 50; i++ {
		d.exec(fmt.Sprintf("INSERT INTO t VALUES (%d, '%s')", i, strings.Repeat("x", 90)))
	}
	for i := 0; i < 5; i++ {
		d.exec(fmt.Sprintf("UPDATE t SET s = 'v%d'", i))
	}
	d.exec("DELETE FROM t WHERE id >= 10")
	before := d.e.log.size
	if err := d.e.Compact(); err != nil {
		t.Fatal(err)
	}
	if d.e.log.size >= before {
		t.Fatalf("log did not shrink: %d -> %d", before, d.e.log.size)
	}
	d.query("SELECT s FROM t WHERE id = 3", "v4;")
	d.exec("INSERT INTO t VALUES (100, 'after')")
	d.crash()
	r := d.exec("SELECT id FROM t ORDER BY id")
	if len(r.Rows) != 11 {
		t.Fatalf("got %d rows after compaction+crash", len(r.Rows))
	}
	// A leftover compaction file from a killed rewrite is ignored.
	os.WriteFile(filepath.Join(d.dir, logTmpName), []byte("junk"), 0o644)
	d.crash()
	d.query("SELECT s FROM t WHERE id = 100", "after;")
}
