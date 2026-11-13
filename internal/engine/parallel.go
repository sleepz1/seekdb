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

// Workers is the largest number of goroutines used for one parallel section
// (exact scans of large tables, conversion of very large INSERTs). It is set
// from the CPUs and the cgroup CPU quota at startup.
var Workers = 1

// The protocol is a single request stream, so apart from those sections only
// the garbage collector uses a second P. Every further P costs memory (its
// allocation cache) and is another thread the runtime wakes for a few
// microseconds of mark work, which under a CPU quota is paid for with
// throttling. The process therefore starts with baseProcs and raises
// GOMAXPROCS to Workers the first time a parallel section really runs.
const (
	baseProcs  = 2
	maxWorkers = 8
)

var (
	procsFromEnv bool
	procsOnce    sync.Once
)

// ConfigureCPU sets GOMAXPROCS and Workers from the CPUs and the container
// CPU quota (cgroup v2 or v1) and starts the GC pacing (see gctune.go).
// GOMAXPROCS and GOGC in the environment still win.
func ConfigureCPU() {
	startGCTuning()
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		n = q
	}
	if n > maxWorkers {
		n = maxWorkers
	}
	if n < 1 {
		n = 1
	}
	if os.Getenv("GOMAXPROCS") != "" {
		procsFromEnv = true
		n = runtime.GOMAXPROCS(0)
	} else {
		runtime.GOMAXPROCS(min(n, baseProcs))
	}
	Workers = n
	if w, err := strconv.Atoi(os.Getenv("MVDB_WORKERS")); err == nil && w >= 1 {
		Workers = w // experiments only
	}
	if w, err := strconv.Atoi(os.Getenv("MVDB_PARMIN")); err == nil && w >= 1 {
		parallelMinWork = w // experiments only
	}
}

// raiseProcs gives the scheduler one P per worker before the first parallel
// section runs.
func raiseProcs() {
	procsOnce.Do(func() {
		if !procsFromEnv && Workers > runtime.GOMAXPROCS(0) {
			runtime.GOMAXPROCS(Workers)
		}
	})
}

// cgroupCPUQuota returns the CPU quota of the container rounded up to whole
// CPUs, or 0 when there is none. Go 1.22 does not look at it.
func cgroupCPUQuota() int {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil { // v2
		f := strings.Fields(string(b))
		if len(f) == 2 {
			return quotaCPUs(f[0], f[1])
		}
		return 0
	}
	for _, dir := range []string{"/sys/fs/cgroup/cpu", "/sys/fs/cgroup/cpu,cpuacct"} { // v1
		q, err1 := os.ReadFile(dir + "/cpu.cfs_quota_us")
		p, err2 := os.ReadFile(dir + "/cpu.cfs_period_us")
		if err1 == nil && err2 == nil {
			return quotaCPUs(strings.TrimSpace(string(q)), strings.TrimSpace(string(p)))
		}
	}
	return 0
}

func quotaCPUs(quota, period string) int {
	q, err1 := strconv.ParseFloat(quota, 64)
	p, err2 := strconv.ParseFloat(period, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0 // "max" (v2) or -1 (v1): no quota
	}
	return int(math.Ceil(q / p))
}

const parallelMinRows = 4096

// insertParChunk is the smallest number of rows of one INSERT or commit
// that is worth a worker of its own. Converting and encoding a row of 128
// dimensions takes a few microseconds, and with the 100-row batches of the
// benchmark two workers measured no faster than one (and many workers
// clearly slower), so only statements of several hundred rows are split.
var insertParChunk = 256

// parallelMinWork is the number of code bytes (rows x dimensions) from which
// the arena scan is split across workers.
var parallelMinWork = 8 << 20

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
	raiseProcs()
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

// parallelFor splits [0,n) into at most Workers contiguous ranges of at
// least minChunk items and runs fn on them concurrently, returning when all
// are done. Waking a worker costs tens of microseconds, so minChunk should
// be worth clearly more than that; smaller inputs run on the caller.
func parallelFor(n, minChunk int, fn func(lo, hi int)) {
	nw := Workers
	if minChunk > 0 && nw > n/minChunk {
		nw = n / minChunk
	}
	if nw <= 1 {
		fn(0, n)
		return
	}
	raiseProcs()
	chunk := (n + nw - 1) / nw
	var wg sync.WaitGroup
	for lo := chunk; lo < n; lo += chunk {
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(lo, hi)
	}
	fn(0, chunk)
	wg.Wait()
}
