package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func benchTable(b *testing.B) (*Session, *Table, [][]float32) {
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
	qs := make([][]float32, 64)
	for i := range qs {
		q := make([]float32, 128)
		for j := range q {
			q[j] = float32(rng.Float64()*2 - 1)
		}
		qs[i] = q
	}
	return s, e.tables["items"], qs
}

func BenchmarkScanUnfiltered(b *testing.B) {
	_, t, qs := benchTable(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := arenaScan(t, nil, 0, len(t.slots), nil, qs[i%len(qs)], 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanPosting(b *testing.B) {
	_, t, qs := benchTable(b)
	list := make([]int32, 0, 1000)
	for i := 0; i < len(t.slots); i += 10 {
		list = append(list, int32(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := arenaScan(t, list, 0, len(list), nil, qs[i%len(qs)], 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDotOnly(b *testing.B) {
	_, t, qs := benchTable(b)
	qq := prepQ8(qs[0])
	var dots [256]int32
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for base := 0; base < len(t.slots); base += 256 {
			m := len(t.slots) - base
			if m > 256 {
				m = 256
			}
			if c := arenaChunkSlots - base&(arenaChunkSlots-1); m > c {
				m = c
			}
			ch := t.qchunks[base>>arenaChunkShift]
			o := (base & (arenaChunkSlots - 1)) * 128
			dotQ8Block(ch[o:o+m*128], &qq, dots[:m])
		}
	}
}
