package engine

import (
	"fmt"
	"testing"

	"minivectordb/internal/sqlparse"
)

// Compare optimized predicates against column-to-column equality, which uses
// the generic evaluator. Include byte-sensitive Unicode and nested predicates.
func TestStringEqualityFilterSemantics(t *testing.T) {
	for _, budget := range []int64{1, 1 << 30} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			d := newTDB(t, Options{CacheBudget: budget})
			d.exec("CREATE TABLE t (id BIGINT PRIMARY KEY, a VARCHAR(32), b VARCHAR(32), v VECTOR(2))")
			d.exec("INSERT INTO t VALUES (1,'甲','甲','[1,0]'),(2,'甲乙','甲','[0,0]'),(3,'','甲','[-1,0]'),(4,'甲 ','甲','[1,0]'),(5,'甲','甲','[1,0]')")
			check := func() {
				for _, pair := range [][2]string{
					{"a = '甲'", "a = b"},
					{"'甲' = a", "b = a"},
					{"NOT (a = '甲')", "NOT (a = b)"},
					{"a = '甲' OR id = 2", "a = b OR id = 2"},
					{"a = '甲' AND id > 1", "a = b AND id > 1"},
				} {
					for _, order := range []string{"ORDER BY id", "ORDER BY L2_DISTANCE(v, '[0,0]') LIMIT 3"} {
						left := rowsString(d.exec("SELECT id FROM t WHERE " + pair[0] + " " + order))
						right := rowsString(d.exec("SELECT id FROM t WHERE " + pair[1] + " " + order))
						if left != right {
							t.Fatalf("%v: %s != %s", pair, left, right)
						}
					}
				}
			}
			check()
			d.exec("BEGIN")
			d.exec("UPDATE t SET a = '甲' WHERE id = 2")
			d.exec("DELETE FROM t WHERE id = 1")
			check()
			d.exec("ROLLBACK")
			check()
			d.restart()
			check()
			d.fail("SELECT id FROM t WHERE a = 1", ErrTypeMismatch)
			// The optimization must retain boolean typing in enclosing comparisons.
			d.fail("SELECT id FROM t WHERE (a = '甲') = (a = b)", ErrTypeMismatch)
		})
	}
}

func TestStringEqualityEmptyAndUnicode(t *testing.T) {
	d := newTDB(t, Options{})
	d.exec("CREATE TABLE t (id BIGINT PRIMARY KEY, a VARCHAR(32))")
	d.exec("INSERT INTO t VALUES (1,''),(2,'é'),(3,'é'),(4,'A'),(5,'a')")
	for _, value := range []string{"", "é", "é", "A", "a", "missing"} {
		for _, reverse := range []bool{false, true} {
			left, right := sqlparse.Expr(&sqlparse.ColRef{Name: "a"}), sqlparse.Expr(&sqlparse.Literal{Kind: sqlparse.LitString, Text: value})
			if reverse {
				left, right = right, left
			}
			compiled, err := (&compiler{s: d.e.tables["t"].schema}).compile(&sqlparse.Binary{Op: "=", L: left, R: right})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []Row{{IntVal(1), StrVal("")}, {IntVal(2), StrVal("é")}, {IntVal(3), StrVal("é")}, {IntVal(4), StrVal("A")}, {IntVal(5), StrVal("a")}} {
				got, err := compiled.eval(row)
				want, refErr := (cmpE{op: "=", l: colE{1}, r: constE{StrVal(value)}}).eval(row)
				if err != refErr || got.I != want.I {
					t.Fatalf("value %q row %v: %v != %v", value, row, got, want)
				}
			}
		}
	}
}
