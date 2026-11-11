package engine

// Vector arena: the committed vectors of a table, stored contiguously by
// slot number in fixed-size chunks of float32 (pointer-free, so the GC never
// scans it, and the TopK scan walks memory sequentially).
//
// Cached rows of an arena table are kept compactly (non-vector columns only,
// see encodeCompact); their decoded vector value points at the slot's arena
// region. A region is only overwritten when
// a new committed version is put into that slot; a single session commits,
// and no SQL statement copies one row's vector into another row, so no live
// row ever observes a region change for a different version.
//
// The arena is kept only while it fits in half of the row-cache budget.
// Past that (tables larger than memory) it is dropped for good and the
// engine falls back to the row cache + pread path.

const arenaChunkShift = 10 // 1024 slots per chunk
const arenaChunkSlots = 1 << arenaChunkShift

func (t *Table) arenaOn() bool { return t.vdim > 0 && !t.arenaOff }

// region returns the arena storage of slot si (it must exist).
func (t *Table) region(si int32) []float32 {
	c := t.vchunks[si>>arenaChunkShift]
	o := int(si&(arenaChunkSlots-1)) * t.vdim
	return c[o : o+t.vdim : o+t.vdim]
}

func (e *Engine) disableArena(t *Table) {
	if t.arenaOff {
		return
	}
	t.arenaOff = true
	e.arenaUsed -= int64(len(t.vchunks)) * t.chunkBytes()
	// Compact cached rows need the arena for their vectors: drop them (they
	// are re-read from the log on demand, like any uncached row).
	for i := range t.slots {
		if sl := &t.slots[i]; sl.crow != nil {
			e.cacheUsed -= sl.cacheSize()
			sl.crow = nil
			t.uncached++
		}
	}
	t.vchunks, t.qchunks, t.mchunks = nil, nil, nil
}

func (t *Table) chunkBytes() int64 {
	n := int64(arenaChunkSlots) * int64(t.vdim) * 4
	if t.useQ8() {
		n += int64(arenaChunkSlots)*int64(t.vdim) + arenaChunkSlots*12
	}
	return n
}

func (t *Table) region8(si int32) []int8 {
	c := t.qchunks[si>>arenaChunkShift]
	o := int(si&(arenaChunkSlots-1)) * t.vdim
	return c[o : o+t.vdim : o+t.vdim]
}

func (t *Table) meta8(si int32) q8meta {
	return t.mchunks[si>>arenaChunkShift][si&(arenaChunkSlots-1)]
}

// arenaStore copies the vector of the committed row full into slot si of
// the arena (growing it by a chunk when needed) and refreshes the int8 copy.
// It returns false when the table has no arena, or when the arena had to be
// disabled because it no longer fits its budget.
func (e *Engine) arenaStore(t *Table, si int32, full Row) bool {
	if !t.arenaOn() {
		return false
	}
	for int(si>>arenaChunkShift) >= len(t.vchunks) {
		sz := t.chunkBytes()
		if e.arenaUsed+sz > e.budget/2 {
			e.disableArena(t)
			return false
		}
		e.arenaUsed += sz
		t.vchunks = append(t.vchunks, make([]float32, arenaChunkSlots*t.vdim))
		if t.useQ8() {
			t.qchunks = append(t.qchunks, make([]int8, arenaChunkSlots*t.vdim))
			t.mchunks = append(t.mchunks, make([]q8meta, arenaChunkSlots))
		}
	}
	reg := t.region(si)
	copy(reg, full[t.schema.VecCol].V)
	if t.useQ8() {
		t.mchunks[si>>arenaChunkShift][si&(arenaChunkSlots-1)] = encodeQ8(t.region8(si), reg)
	}
	return true
}
