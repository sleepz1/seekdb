package engine

import (
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"minivectordb/internal/sqlparse"
)

// Workers is the number of goroutines used for large exact scans. It is set
// from the cgroup CPU quota at startup (Go 1.22 does not do this itself).
var Workers = 1

// ConfigureCPU sets GOMAXPROCS and Workers from the container CPU quota.
func ConfigureCPU() {
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		n = q
	}
	if n < 1 {
		n = 1
	}
	runtime.GOMAXPROCS(n)
	Workers = n
}

func cgroupCPUQuota() int {
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || period <= 0 {
		return 0
	}
	return int(math.Ceil(quota / period))
}

const parallelMinRows = 4096

// parallelTopKL2 splits the slot array across workers when every row is
// cached and no uncommitted changes touch the table. Each worker keeps its
// own bounded heap; the merge uses the same (distance, pk) order, so results
// are identical to the sequential scan. ok=false means "use sequential".
func (s *Session) parallelTopKL2(t *Table, where sqlparse.Expr, key l2E, k int) ([]candidate, bool, error) {
	if Workers < 2 || len(t.slots) < parallelMinRows || len(s.txnLayer(t)) > 0 {
		return nil, false, nil
	}
	if where != nil {
		if _, ok := pkEquality(where, t.schema); ok {
			return nil, false, nil
		}
	}
	for i := range t.slots {
		if t.slots[i].used && t.slots[i].row == nil {
			return nil, false, nil
		}
	}
	var filter cexpr
	if where != nil {
		var err error
		if filter, err = (&compiler{s: t.schema}).compile(where); err != nil {
			return nil, true, err
		}
	}
	nw := Workers
	chunk := (len(t.slots) + nw - 1) / nw
	results := make([][]l2Hit, nw)
	errs := make([]error, nw)
	var wg sync.WaitGroup
	for w := 0; w < nw; w++ {
		lo, hi := w*chunk, (w+1)*chunk
		if hi > len(t.slots) {
			hi = len(t.slots)
		}
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			results[w], errs[w] = scanTopK(t.slots[lo:hi], filter, key, k)
		}(w, lo, hi)
	}
	wg.Wait()
	var all []l2Hit
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
		out[i] = candidate{pk: h.pk, row: h.row}
	}
	return out, true, nil
}

func scanTopK(slots []slot, filter cexpr, key l2E, k int) ([]l2Hit, error) {
	h := make([]l2Hit, 0, k)
	bound := math.Inf(1)
	for i := range slots {
		sl := &slots[i]
		if !sl.used {
			continue
		}
		r := sl.row
		if filter != nil {
			v, err := filter.eval(r)
			if err != nil {
				return nil, err
			}
			ok, err := truth(v)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		sq, full := l2sqBounded(r[key.col].V, key.q, bound)
		if !full {
			continue
		}
		d := math.Sqrt(sq)
		if math.IsInf(d, 0) || math.IsNaN(d) {
			return nil, newErr(ErrOutOfRange, "distance out of range")
		}
		c := l2Hit{d: d, pk: sl.pk, row: r}
		if len(h) < k {
			h = append(h, c)
			siftUp(h, len(h)-1)
		} else if h[0].worse(c) {
			h[0] = c
			siftDown(h, 0)
		} else {
			continue
		}
		if len(h) == k {
			bound = h[0].d * h[0].d * (1 + 1e-9)
		}
	}
	return h, nil
}
