package engine

import (
	"math"
	"os"
	"sync/atomic"
	"time"
)

// Helper thread for the TopK scan.
//
// The protocol is one request at a time, so a query can use a second CPU
// only if handing work to it costs much less than the scan (about 60 us for
// 10,000 rows of 128 dimensions). Waking a parked goroutine costs tens of
// microseconds, which is why splitting with fresh goroutines did not pay
// (round 3). Here the server calls Prewake as soon as it has read a request
// line that looks like a SELECT, so the helper wakes while the request is
// still being decoded and parsed. It then spins for helperSpin waiting for
// a task: the second half of the scan, handed over with two atomic stores.
// The two halves exchange their k-th distance bounds while they run (see
// arenaScanPeer) and the merge is the same as for the goroutine split, so
// results are identical to the sequential scan.
//
// The helper only exists when the CPU quota allows two workers; spinning is
// bounded by helperSpin after the latest request, so an idle server parks
// it. MVDB_NOHELPER=1 turns it off.

const (
	helperIdle    = 0 // parked on wake
	helperSpin    = 1 // awake, waiting for a task
	helperClaimed = 2 // the query thread is filling in a task
	helperTask    = 3 // task ready
	helperDone    = 4 // result ready
)

// helperMinWork is the number of code bytes (rows x dimensions) from which
// a scan is split with the helper.
var helperMinWork = 512 * 128

// helperWindow is how long the helper spins after the latest Prewake.
var helperWindow = int64(300 * time.Microsecond)

type scanHelper struct {
	state atomic.Int32
	last  atomic.Int64 // clock() at the latest Prewake
	wake  chan struct{}

	t      *Table
	list   []int32
	lo, hi int
	q      []float32
	k      int
	toMain atomic.Uint64 // helper's k-th bound (float64 bits)
	toHelp atomic.Uint64 // query thread's k-th bound
	res    []arenaHit
	err    error
}

var helper *scanHelper

var clockBase = time.Now()

func clock() int64 { return int64(time.Since(clockBase)) }

// startHelper is called once Workers is known.
func startHelper() {
	if Workers < 2 || os.Getenv("MVDB_NOHELPER") == "1" || helper != nil {
		return
	}
	helper = &scanHelper{wake: make(chan struct{}, 1)}
	go helper.run()
}

// Prewake tells the helper that a query may follow shortly.
func Prewake() {
	h := helper
	if h == nil {
		return
	}
	h.last.Store(clock())
	if h.state.Load() == helperIdle {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
}

func (h *scanHelper) run() {
	for range h.wake {
		if !h.state.CompareAndSwap(helperIdle, helperSpin) {
			continue
		}
		for spins := 0; ; spins++ {
			switch h.state.Load() {
			case helperTask:
				h.res, h.err = arenaScanPeer(h.t, h.list, h.lo, h.hi, nil, h.q, h.k, &h.toHelp, &h.toMain)
				h.state.Store(helperDone)
				continue
			case helperSpin:
				if spins&63 == 0 && clock()-h.last.Load() > helperWindow {
					if h.state.CompareAndSwap(helperSpin, helperIdle) {
						// A Prewake that saw helperSpin just before the
						// swap sent no wake-up: take it back.
						if clock()-h.last.Load() <= helperWindow && h.state.CompareAndSwap(helperIdle, helperSpin) {
							continue
						}
						goto parked
					}
				}
			}
		}
	parked:
	}
}

// helperScan runs the first half of the scan on the calling goroutine and
// the second half on the helper, if it is awake and free. ok=false: run
// the scan alone.
func helperScan(t *Table, list []int32, n int, q []float32, k int, results *[][]arenaHit, errs *[]error) bool {
	h := helper
	if h == nil || !h.state.CompareAndSwap(helperSpin, helperClaimed) {
		return false
	}
	mid := n / 2
	if list == nil {
		// keep the split on a 256-slot block boundary
		mid &^= 255
	}
	inf := math.Float64bits(math.Inf(1))
	h.t, h.list, h.lo, h.hi, h.q, h.k = t, list, mid, n, q, k
	h.toMain.Store(inf)
	h.toHelp.Store(inf)
	h.res, h.err = nil, nil
	h.state.Store(helperTask)
	r0, e0 := arenaScanPeer(t, list, 0, mid, nil, q, k, &h.toMain, &h.toHelp)
	for h.state.Load() != helperDone {
	}
	r1, e1 := h.res, h.err
	h.t, h.list, h.q, h.res, h.err = nil, nil, nil, nil, nil
	h.state.Store(helperSpin)
	*results = [][]arenaHit{r0, r1}
	*errs = []error{e0, e1}
	return true
}
