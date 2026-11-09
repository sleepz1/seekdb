package db

import (
	"fmt"
	"math/bits"
	"regexp"
	"strings"
)

// Sign-LSH: up to eight hyperplanes through the origin form coarse buckets.
// Bucket membership contains row offsets, rebuilt after each successful mutation.
// Metadata is persisted; buckets are reconstructed on clone/replay/snapshot load.
type VectorIndex struct {
	Name    string
	Column  string
	buckets map[uint8][]int
}

var indexRE = regexp.MustCompile(`(?is)^CREATE\s+VECTOR\s+INDEX\s+(\w+)\s+ON\s+(\w+)\s*\(\s*(\w+)\s*\)$`)

func createIndex(state State, name, table, column string) error {
	name, table, column = strings.ToLower(name), strings.ToLower(table), strings.ToLower(column)
	for _, t := range state {
		for _, idx := range t.Indexes {
			if idx.Name == name {
				return fmt.Errorf("index exists")
			}
		}
	}
	t := state[table]
	if t == nil {
		return fmt.Errorf("unknown table")
	}
	i := columnIndex(t, column)
	if i < 0 || t.Columns[i].Type != "VECTOR" {
		return fmt.Errorf("index requires vector column")
	}
	t.Indexes = append(t.Indexes, VectorIndex{Name: name, Column: column})
	return nil
}
func bucket(v []float32) uint8 {
	var key uint8
	for i, x := range v {
		if i == 8 {
			break
		}
		if x >= 0 {
			key |= 1 << i
		}
	}
	return key
}
func rebuildIndexes(t *Table) {
	for i := range t.Indexes {
		idx := &t.Indexes[i]
		idx.buckets = map[uint8][]int{}
		col := columnIndex(t, idx.Column)
		if col < 0 || t.Columns[col].Type != "VECTOR" {
			continue
		}
		for n := 0; n < t.rowCount(); n++ {
			row := t.rowAt(n)
			key := bucket(row[col].([]float32))
			idx.buckets[key] = append(idx.buckets[key], n)
		}
	}
}
func vectorIndex(t *Table, col int) *VectorIndex {
	for i := range t.Indexes {
		if t.Indexes[i].Column == t.Columns[col].Name {
			return &t.Indexes[i]
		}
	}
	return nil
}

// Probe complete Hamming shells through radius four, then expand until the
// post-filter budget is met or all buckets are exhausted. Completing shells
// avoids bucket-order bias. Radius four visits 163/256 signatures, trading more
// distance evaluations for recall on independent high-dimensional data. Eight
// signs still omit remaining coordinates; this is not a general recall guarantee.
// Exact mode never uses these candidates.
func indexCandidateOffsets(t *Table, idx *VectorIndex, q []float32, preds []predicate, limit int) []int {
	key := bucket(q)
	// Cap before multiplying to avoid overflow for arbitrarily large SQL LIMITs.
	budget := t.rowCount()
	if limit >= 0 && limit <= budget/8 {
		budget = limit * 8
	}
	if limit == 0 {
		return []int{}
	}
	out := []int{}
	for radius := 0; radius <= 8; radius++ {
		for b := 0; b < 256; b++ {
			if bits.OnesCount8(uint8(b)^key) != radius {
				continue
			}
			for _, n := range idx.buckets[uint8(b)] {
				row := t.rowAt(n)
				if matches(row, preds) {
					out = append(out, n)
				}
			}
		}
		if radius >= 4 && len(out) >= budget {
			return out
		}
	}
	return out
}
