package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func benchVec(rng *rand.Rand, dim int) string {
	parts := make([]string, dim)
	for i := range parts {
		parts[i] = fmt.Sprintf("%.6f", rng.Float64()*2-1)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// BenchmarkTopK mirrors one query of the contest workload (10k x 128, K=10).
func BenchmarkTopK(b *testing.B) {
	e, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	s := e.NewSession("exact")
	if _, err := s.ExecSQL("CREATE TABLE items (id BIGINT PRIMARY KEY, cat VARCHAR(16), embedding VECTOR(128))"); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 10000; i += 100 {
		var rows []string
		for j := i; j < i+100; j++ {
			rows = append(rows, fmt.Sprintf("(%d,'c%d','%s')", j, j%10, benchVec(rng, 128)))
		}
		if _, err := s.ExecSQL("INSERT INTO items VALUES " + strings.Join(rows, ",")); err != nil {
			b.Fatal(err)
		}
	}
	qs := make([]string, 64)
	for i := range qs {
		where := ""
		if i%2 == 1 {
			where = fmt.Sprintf(" WHERE cat = 'c%d'", i%10)
		}
		qs[i] = fmt.Sprintf("SELECT id, L2_DISTANCE(embedding, '%s') AS distance FROM items%s ORDER BY distance LIMIT 10", benchVec(rng, 128), where)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ExecSQL(qs[i%len(qs)]); err != nil {
			b.Fatal(err)
		}
	}
}
