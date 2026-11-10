package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"minivectordb/internal/sqlparse"
)

// TestFastTopKMatchesReference compares the arena/SIMD TopK with the
// reference scan (arena path disabled) on varied dimensions, value scales,
// near-ties, exact ties, filters, deletes and updates, for every K.
func TestFastTopKMatchesReference(t *testing.T) {
	old := Workers
	defer func() { Workers = old; fastPath = true }()
	for _, dim := range []int{1, 3, 8, 13, 32, 128, 300} {
		for _, scale := range []string{"ints", "unit", "tiny", "huge", "near"} {
			d := newTDB(t, Options{})
			d.exec(fmt.Sprintf("CREATE TABLE v (id BIGINT PRIMARY KEY, cat VARCHAR(4), n BIGINT, emb VECTOR(%d))", dim))
			rng := rand.New(rand.NewSource(int64(dim)*31 + int64(len(scale))))
			base := make([]float32, dim)
			for i := range base {
				base[i] = float32(rng.NormFloat64())
			}
			vec := func() string {
				v := make([]float32, dim)
				for i := range v {
					switch scale {
					case "ints":
						v[i] = float32(rng.Intn(3))
					case "unit":
						v[i] = float32(rng.Float64()*2 - 1)
					case "tiny":
						v[i] = float32((rng.Float64()*2 - 1) * 1e-20)
					case "huge":
						v[i] = float32((rng.Float64()*2 - 1) * 1e18)
					case "near": // all points almost equidistant from base
						v[i] = base[i] + float32(rng.Intn(2))*1e-6
					}
				}
				return FormatVector(v)
			}
			rows := 5000
			for i := 0; i < rows; i += 500 {
				var vals []string
				for j := i; j < i+500; j++ {
					vals = append(vals, fmt.Sprintf("(%d,'c%d',%d,'%s')", j, j%4, j%7, vec()))
				}
				d.exec("INSERT INTO v VALUES " + strings.Join(vals, ","))
			}
			d.exec("DELETE FROM v WHERE id < 300")
			d.exec(fmt.Sprintf("UPDATE v SET emb = '%s' WHERE id = 4000", vec()))
			for q := 0; q < 48; q++ {
				if q == 24 { // postings must follow committed changes
					d.exec("UPDATE v SET cat = 'c1' WHERE id < 1500")
					d.exec("DELETE FROM v WHERE n = 3 AND id > 4500")
					d.exec(fmt.Sprintf("INSERT INTO v VALUES (90001,'c1',3,'%s')", vec()))
				}
				where := []string{"", " WHERE cat = 'c1'", " WHERE n = 3", " WHERE cat = 'c2' AND id > 2500", " WHERE 'c0' = cat"}[q%5]
				sql := fmt.Sprintf("SELECT id, L2_DISTANCE(emb, '%s') AS dist FROM v%s ORDER BY dist LIMIT %d", vec(), where, []int{1, 10, 37, 6000}[q%4])
				fastPath = false
				Workers = 1
				ref := exactRows(d.exec(sql))
				fastPath = true
				for _, w := range []int{1, 2, 3} {
					Workers = w
					if got := exactRows(d.exec(sql)); got != ref {
						t.Fatalf("dim=%d scale=%s workers=%d %s:\n ref %.300s\n got %.300s", dim, scale, w, where, ref, got)
					}
				}
			}
		}
	}
}

// exactRows renders results with full float precision (bit-exact compare).
func exactRows(r *Result) string {
	var sb strings.Builder
	for _, row := range r.Rows {
		for _, v := range row {
			fmt.Fprintf(&sb, "%d|%s|%b|", v.I, v.S, v.F)
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

// TestFastTopKTransactionAndOverflow covers the fallbacks: pending
// transaction changes use the reference path, and a float32 overflow in the
// estimate never prunes a row.
func TestFastTopKTransactionAndOverflow(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, emb VECTOR(2))")
	d.exec("INSERT INTO v VALUES (1,'[3e38,3e38]'),(2,'[-3e38,-3e38]'),(3,'[0,0]')")
	d.query("SELECT id FROM v ORDER BY L2_DISTANCE(emb, '[3e38,3e38]') LIMIT 2", "1;3;")
	d.query("SELECT id FROM v ORDER BY L2_DISTANCE(emb, '[-3e38,-3e38]') LIMIT 3", "2;3;1;")
	d.exec("BEGIN")
	d.exec("UPDATE v SET emb = '[3e38,3e38]' WHERE id = 3")
	d.query("SELECT id FROM v ORDER BY L2_DISTANCE(emb, '[3e38,3e38]') LIMIT 2", "1;3;")
	d.exec("ROLLBACK")
	d.query("SELECT id FROM v ORDER BY L2_DISTANCE(emb, '[3e38,3e38]') LIMIT 2", "1;3;")
	if math.IsInf(float64(l2sq32([]float32{3e38}, []float32{-3e38})), 1) == false {
		t.Fatal("expected float32 overflow in the estimate")
	}
}

// TestPostingPathIsUsed guards the fast filtered path against compiler
// changes (e.g. a specialised equality node) silently disabling it.
func TestPostingPathIsUsed(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec("CREATE TABLE v (id BIGINT PRIMARY KEY, cat VARCHAR(4), n BIGINT, emb VECTOR(16))")
	d.exec("INSERT INTO v VALUES (1,'a',5,'[1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]'),(2,'b',6,'[0,1,0,0,0,0,0,0,0,0,0,0,0,0,0,0]')")
	tb := d.s.e.tables["v"]
	for _, w := range []string{"cat = 'a'", "'b' = cat", "n = 5", "6 = n"} {
		ce, err := (&compiler{s: tb.schema}).compile(mustWhere(t, "SELECT id FROM v WHERE "+w))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := eqFilter(ce, tb.schema); !ok {
			t.Fatalf("%s: equality filter not recognised (%T)", w, ce)
		}
	}
	d.query("SELECT id FROM v WHERE cat = 'b' ORDER BY L2_DISTANCE(emb, '[1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]') LIMIT 5", "2;")
	if len(tb.postings) == 0 {
		t.Fatal("posting list was not built")
	}
}

func mustWhere(t *testing.T, sql string) sqlparse.Expr {
	st, err := sqlparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return st.(*sqlparse.Select).Where
}
