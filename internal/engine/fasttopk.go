package engine

import (
	"math"
	"sort"
	"sync"

	"minivectordb/internal/sqlparse"
)

// fastTopKL2 is the exact filtered TopK over the vector arena.
//
// For every visible row that passes the filter, a float32 SIMD estimate of
// the squared distance is compared with the current k-th exact distance.
// Rows whose estimate is provably farther (with a margin that covers the
// float32 rounding error) are skipped; all others are recomputed exactly in
// float64 with l2sqBounded, exactly as the reference path does. The result
// (rows, order, distances, pk tie-breaks) is therefore identical to
// topKL2's sequential scan; only the work for far rows is cheaper.
//
// ok=false means the fast path does not apply (no arena, pending
// transaction changes on the table, uncached rows with a filter, ...).
// fastPath can be switched off by tests to compare against the reference scan.
var fastPath = true

func (s *Session) fastTopKL2(t *Table, where sqlparse.Expr, key l2E, k int) ([]candidate, bool, error) {
	if !fastPath || !t.arenaOn() || key.col != t.schema.VecCol || len(key.q) != t.vdim || len(s.txnLayer(t)) > 0 {
		return nil, false, nil
	}
	var filter func(Row) (bool, error)
	var ce cexpr
	if where != nil {
		if _, ok := pkEquality(where, t.schema); ok {
			return nil, false, nil // point lookup path is faster
		}
		if t.uncached > 0 {
			return nil, false, nil
		}
		var err error
		if ce, err = (&compiler{s: t.schema}).compile(where); err != nil {
			return nil, true, err
		}
		filter = fastFilter(ce)
	}
	// "col = constant" on an indexed-able column: scan only the posting
	// list of matching slots (exactly the rows the filter accepts).
	var list []int32
	if where != nil {
		if col, v, ok := eqFilter(ce, t.schema); ok {
			list = t.posting(col, v)
			filter = nil
			if list == nil {
				return nil, true, nil // no row matches
			}
		}
	}
	n := len(t.slots)
	if list != nil {
		n = len(list)
	}
	nw := Workers
	if n < parallelMinRows {
		nw = 1
	}
	chunk := (n + nw - 1) / nw
	results := make([][]arenaHit, nw)
	errs := make([]error, nw)
	if nw == 1 {
		results[0], errs[0] = arenaScan(t, list, 0, n, filter, key.q, k)
	} else {
		var wg sync.WaitGroup
		for w := 0; w < nw; w++ {
			lo, hi := w*chunk, (w+1)*chunk
			if hi > n {
				hi = n
			}
			if lo >= hi {
				continue
			}
			wg.Add(1)
			go func(w, lo, hi int) {
				defer wg.Done()
				results[w], errs[w] = arenaScan(t, list, lo, hi, filter, key.q, k)
			}(w, lo, hi)
		}
		wg.Wait()
	}
	var all []arenaHit
	for w := range results {
		if errs[w] != nil {
			return nil, true, errs[w]
		}
		all = append(all, results[w]...)
	}
	sort.Slice(all, func(i, j int) bool { return all[j].worse(all[i]) })
	if len(all) > k {
		all = all[:k]
	}
	out := make([]candidate, len(all))
	for i, h := range all {
		r, err := s.e.loadRow(t, h.si)
		if err != nil {
			return nil, true, err
		}
		out[i] = candidate{pk: h.pk, row: r}
	}
	return out, true, nil
}

type arenaHit struct {
	d  float64
	pk int64
	si int32
}

func (a arenaHit) worse(b arenaHit) bool {
	if a.d != b.d {
		return a.d > b.d
	}
	return a.pk > b.pk
}

// pruneMargin bounds the relative error of the float32 estimate of a sum of
// n non-negative squared differences (rounding of each difference, square
// and addition along a reduction tree of depth <= n/32+12), with ample slack.
func pruneMargin(n int) float64 { return 1e-5 + float64(n)*4e-9 }

// arenaScan scans slots lo..hi-1, or list[lo:hi] when list is non-nil.
func arenaScan(t *Table, list []int32, lo, hi int, filter func(Row) (bool, error), q []float32, k int) ([]arenaHit, error) {
	sc := scanState{t: t, filter: filter, q: q, k: k,
		h: make([]arenaHit, 0, k), bound: math.Inf(1), approxBound: math.Inf(1), kth: math.Inf(1),
		rel: 1 + pruneMargin(len(q)), q8: t.useQ8()}
	if sc.q8 {
		sc.qq = prepQ8(q)
	}
	if list == nil && sc.q8 {
		// Block kernel: int8 dot products for up to 256 consecutive slots.
		var dots [256]int32
		dim := t.vdim
		for base := lo; base < hi; {
			m := hi - base
			if m > len(dots) {
				m = len(dots)
			}
			if c := arenaChunkSlots - base&(arenaChunkSlots-1); m > c {
				m = c
			}
			ch := t.qchunks[base>>arenaChunkShift]
			o := (base & (arenaChunkSlots - 1)) * dim
			dotQ8Block(ch[o:o+m*dim], sc.qq.e, dots[:m])
			ms := t.mchunks[base>>arenaChunkShift][base&(arenaChunkSlots-1):]
			var seeded [256]bool
			if base == lo && k <= 32 && m >= 8*k {
				// Seed the heap with the k rows of the first block that look
				// closest by the int8 estimate, so that the bound is tight
				// from the start and far fewer rows need an exact check.
				// Order of visits does not affect the result.
				for _, j := range sc.qq.closest(ms[:m], dots[:m], k) {
					seeded[j] = true
					if err := sc.visit(base+j, false); err != nil {
						return nil, err
					}
				}
			}
			for j := 0; j < m; j++ {
				if seeded[j] || sc.qq.prune(ms[j], dots[j], sc.kth) {
					continue
				}
				if err := sc.visit(base+j, false); err != nil {
					return nil, err
				}
			}
			base += m
		}
		return sc.h, nil
	}
	for j := lo; j < hi; j++ {
		i := j
		if list != nil {
			i = int(list[j])
		}
		if err := sc.visit(i, true); err != nil {
			return nil, err
		}
	}
	return sc.h, nil
}

type scanState struct {
	t           *Table
	filter      func(Row) (bool, error)
	q           []float32
	k           int
	h           []arenaHit
	bound       float64 // exact squared distance of the k-th hit (with margin)
	approxBound float64 // bound for the float32 estimate
	kth         float64 // k-th exact distance (with margin)
	rel         float64
	q8          bool
	qq          q8query
}

// visit considers slot i; prune=true applies the per-row estimate first.
func (sc *scanState) visit(i int, prune bool) error {
	t := sc.t
	sl := &t.slots[i]
	if !sl.used {
		return nil
	}
	if sc.filter != nil {
		ok, err := sc.filter(t.cachedRow(i))
		if err != nil || !ok {
			return err
		}
	}
	if prune && sc.q8 {
		// Lower bound from the int8 copy (see q8.go); skip only rows
		// strictly farther than the current k-th exact distance.
		if !math.IsInf(sc.kth, 1) && sc.qq.prune(t.meta8(int32(i)), dotQ8(t.region8(int32(i)), sc.qq.e), sc.kth) {
			return nil
		}
	}
	v := t.region(int32(i))
	if prune && !sc.q8 {
		if est := float64(l2sq32(v, sc.q)); est > sc.approxBound && !math.IsInf(est, 1) {
			return nil // provably farther than the k-th hit
		}
	}
	sq, full := l2sqBounded(v, sc.q, sc.bound)
	if !full {
		return nil
	}
	d := math.Sqrt(sq)
	if math.IsInf(d, 0) || math.IsNaN(d) {
		return newErr(ErrOutOfRange, "distance out of range")
	}
	c := arenaHit{d: d, pk: sl.pk, si: int32(i)}
	h := sc.h
	if len(h) < sc.k {
		h = append(h, c)
		arenaUp(h, len(h)-1)
	} else if h[0].worse(c) {
		h[0] = c
		arenaDown(h, 0)
	} else {
		return nil
	}
	sc.h = h
	if len(h) == sc.k {
		sc.bound = h[0].d * h[0].d * (1 + 1e-9)
		sc.approxBound = sc.bound*sc.rel + 1e-36
		sc.kth = h[0].d*(1+1e-9) + 1e-30
	}
	return nil
}

func arenaUp(h []arenaHit, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h[i].worse(h[p]) {
			return
		}
		h[i], h[p] = h[p], h[i]
		i = p
	}
}

func arenaDown(h []arenaHit, i int) {
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

// eqFilter recognises "column = constant" where the constant has the
// column's own kind (BIGINT/INT or VARCHAR/string), so that posting-list
// membership is exactly the filter's result.
func eqFilter(ce cexpr, sch *Schema) (int, Value, bool) {
	if c, ok := ce.(strEqE); ok && sch.Columns[c.col].Type == sqlparse.TypeVarchar {
		return c.col, StrVal(c.value), true
	}
	c, ok := ce.(cmpE)
	if !ok || c.op != "=" {
		return 0, Value{}, false
	}
	col, okc := c.l.(colE)
	lit, okl := c.r.(constE)
	if !okc || !okl {
		col, okc = c.r.(colE)
		lit, okl = c.l.(constE)
	}
	if !okc || !okl {
		return 0, Value{}, false
	}
	switch sch.Columns[col.idx].Type {
	case sqlparse.TypeBigint:
		if lit.v.K == KInt {
			return col.idx, lit.v, true
		}
	case sqlparse.TypeVarchar:
		if lit.v.K == KStr {
			return col.idx, lit.v, true
		}
	}
	return 0, Value{}, false
}

// posting returns the slots (ascending) whose committed row has value v in
// column col, building the per-column lists lazily. They are discarded by
// any commit to the table (t.version), so they never go stale. Requires
// every row to be cached.
func (t *Table) posting(col int, v Value) []int32 {
	p := t.postings[col]
	if p == nil || p.version != t.version {
		p = &postingList{version: t.version}
		if t.schema.Columns[col].Type == sqlparse.TypeVarchar {
			p.str = make(map[string][]int32)
		} else {
			p.num = make(map[int64][]int32)
		}
		for i := range t.slots {
			sl := &t.slots[i]
			if !sl.used {
				continue
			}
			x := t.cachedRow(i)[col]
			if p.str != nil {
				p.str[x.S] = append(p.str[x.S], int32(i))
			} else {
				p.num[x.I] = append(p.num[x.I], int32(i))
			}
		}
		if t.postings == nil {
			t.postings = make(map[int]*postingList)
		}
		t.postings[col] = p
	}
	if p.str != nil {
		return p.str[v.S]
	}
	return p.num[v.I]
}

type postingList struct {
	version uint64
	str     map[string][]int32
	num     map[int64][]int32
}

// fastFilter specialises "column = constant" (either side) on BIGINT or
// VARCHAR columns, the shape of the benchmark's filtered queries; anything
// else is evaluated through the compiled expression.
func fastFilter(ce cexpr) func(Row) (bool, error) {
	if c, ok := ce.(strEqE); ok {
		return func(r Row) (bool, error) { return r[c.col].S == c.value, nil }
	}
	if c, ok := ce.(cmpE); ok && c.op == "=" {
		col, okc := c.l.(colE)
		lit, okl := c.r.(constE)
		if !okc || !okl {
			col, okc = c.r.(colE)
			lit, okl = c.l.(constE)
		}
		if okc && okl {
			idx := col.idx
			switch lit.v.K {
			case KStr:
				want := lit.v.S
				return func(r Row) (bool, error) {
					if v := r[idx]; v.K == KStr {
						return v.S == want, nil
					}
					return evalTruth(ce, r)
				}
			case KInt:
				want := lit.v.I
				return func(r Row) (bool, error) {
					if v := r[idx]; v.K == KInt {
						return v.I == want, nil
					}
					return evalTruth(ce, r)
				}
			}
		}
	}
	return func(r Row) (bool, error) { return evalTruth(ce, r) }
}

func evalTruth(ce cexpr, r Row) (bool, error) {
	v, err := ce.eval(r)
	if err != nil {
		return false, err
	}
	return truth(v)
}
