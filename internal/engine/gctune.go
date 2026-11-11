package engine

import (
	"os"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
)

// GC pacing.
//
// The platform scores the peak memory of the container. With a fixed GOGC
// the heap may grow to live*(1+GOGC/100) before a collection starts, so the
// slack grows with the table. Most of the live heap is the vector arena,
// which holds no pointers and costs nothing to mark, so collecting more
// often is cheap. tuneGC aims the slack at gcSlackBase + live/16 bytes
// instead: about 2 MiB at 10k x 128 rather than 3.6 MiB.
//
// It reads the live heap of the last collection from runtime/metrics (no
// stop-the-world) and only calls SetGCPercent when the value moves by more
// than a tenth. A GOGC environment variable switches the tuning off.

const gcSlackBase = 3 << 19 // 1.5 MiB

// The percent stays within [20, 40]: never looser than the fixed 40 used
// before, and never so tight that a large pointer-rich heap (row cache of a
// table without arena) is marked more than twice as often as before.
const (
	gcPercentMin = 20
	gcPercentMax = 40
)

var (
	gcTuning   bool
	gcPercent  = 40
	gcSample   = []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	gcCalls    uint32
	gcSlackEnv = -1
)

func startGCTuning() {
	if os.Getenv("GOGC") != "" {
		return
	}
	debug.SetGCPercent(gcPercent)
	gcTuning = true
	if v, err := strconv.Atoi(os.Getenv("MVDB_GCSLACK")); err == nil && v >= 0 {
		gcSlackEnv = v // experiments only: slack base in bytes
	}
}

// tuneGC adjusts the GC percent to the current live heap. It is called
// every few statements and whenever the arena grows.
func tuneGC() {
	if !gcTuning {
		return
	}
	metrics.Read(gcSample)
	if gcSample[0].Value.Kind() != metrics.KindUint64 {
		return
	}
	live := gcSample[0].Value.Uint64()
	if live == 0 {
		return // no collection yet
	}
	base := uint64(gcSlackBase)
	if gcSlackEnv >= 0 {
		base = uint64(gcSlackEnv)
	}
	p := int((base + live/16) * 100 / live)
	if p > gcPercentMax {
		p = gcPercentMax
	} else if p < gcPercentMin {
		p = gcPercentMin
	}
	if d := p - gcPercent; d*10 > gcPercent || -d*10 > gcPercent {
		gcPercent = p
		debug.SetGCPercent(p)
	}
}

// tuneGCEvery calls tuneGC on every 32nd call.
func tuneGCEvery() {
	if gcCalls++; gcCalls&31 == 0 {
		tuneGC()
	}
}
