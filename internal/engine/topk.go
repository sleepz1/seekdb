package engine

import (
	"math"

	"minivectordb/internal/sqlparse"
)

// singleL2Key reports whether the ORDER BY is exactly one ascending
// L2_DISTANCE(vector_column, constant), the shape of vector TopK queries.
func singleL2Key(keys []orderKey) (l2E, bool) {
	if len(keys) != 1 || keys[0].desc {
		return l2E{}, false
	}
	l2, ok := keys[0].ex.(l2E)
	return l2, ok
}

type l2Hit struct {
	d   float64 // distance
	pk  int64
	row Row
}

// worse reports whether a ranks after b (larger distance, then larger pk).
func (a l2Hit) worse(b l2Hit) bool {
	if a.d != b.d {
		return a.d > b.d
	}
	return a.pk > b.pk
}

// topKL2 is the exact filtered TopK: the filter is applied to every visible
// row first, then a bounded max-heap keeps the k nearest by squared distance
// (monotonic with distance) and primary key. No per-row allocation.
func (s *Session) topKL2(t *Table, where sqlparse.Expr, key l2E, k int) ([]candidate, error) {
	if out, ok, err := s.parallelTopKL2(t, where, key, k); ok || err != nil {
		return out, err
	}
	h := make([]l2Hit, 0, k)
	q := key.q
	col := key.col
	bound := math.Inf(1) // squared-distance pruning bound once the heap is full
	err := s.matching(t, where, nil, func(pk int64, r Row) error {
		sq, full := l2sqBounded(r[col].V, q, bound)
		if !full {
			return nil // provably farther than the current k-th result
		}
		// Rank by the same sqrt value that is returned, so ties are decided
		// exactly as the output shows them.
		d := math.Sqrt(sq)
		if math.IsInf(d, 0) || math.IsNaN(d) {
			return newErr(ErrOutOfRange, "distance out of range")
		}
		c := l2Hit{d: d, pk: pk, row: r}
		if len(h) < k {
			h = append(h, c)
			siftUp(h, len(h)-1)
		} else if h[0].worse(c) {
			h[0] = c
			siftDown(h, 0)
		} else {
			return nil
		}
		if len(h) == k {
			// A margin above worst^2 guarantees pruned rows are strictly
			// farther even after sqrt rounding, so pk tie-breaks are intact.
			bound = h[0].d * h[0].d * (1 + 1e-9)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Pop into ascending order.
	out := make([]candidate, len(h))
	for i := len(h) - 1; i >= 0; i-- {
		out[i] = candidate{pk: h[0].pk, row: h[0].row}
		h[0] = h[len(h)-1]
		h = h[:len(h)-1]
		if len(h) > 0 {
			siftDown(h, 0)
		}
	}
	return out, nil
}

// max-heap on "worse"
func siftUp(h []l2Hit, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h[i].worse(h[p]) {
			return
		}
		h[i], h[p] = h[p], h[i]
		i = p
	}
}

func siftDown(h []l2Hit, i int) {
	n := len(h)
	for {
		l := 2*i + 1
		if l >= n {
			return
		}
		m := l
		if r := l + 1; r < n && h[r].worse(h[l]) {
			m = r
		}
		if !h[m].worse(h[i]) {
			return
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}
