package engine

// Vector arena: the committed vectors of a table, stored contiguously by
// slot number in fixed-size chunks of float32 (pointer-free, so the GC never
// scans it, and the TopK scan walks memory sequentially).
//
// Cached rows do not keep a second copy of their vector: their vector value
// is re-pointed at the slot's arena region. A region is only overwritten when
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

// arenaPut stores the vector of row (the full committed row, never nil when
// the arena is on) into slot si and returns the row to cache, re-pointed at
// the arena (a shallow copy; the caller's row is not modified).
func (e *Engine) arenaPut(t *Table, si int32, full Row, cache Row) Row {
	if !t.arenaOn() {
		return cache
	}
	if full == nil {
		e.disableArena(t)
		return cache
	}
	for int(si>>arenaChunkShift) >= len(t.vchunks) {
		sz := t.chunkBytes()
		if e.arenaUsed+sz > e.budget/2 {
			e.disableArena(t)
			return cache
		}
		e.arenaUsed += sz
		t.vchunks = append(t.vchunks, make([]float32, arenaChunkSlots*t.vdim))
		if t.useQ8() {
			t.qchunks = append(t.qchunks, make([]int8, arenaChunkSlots*t.vdim))
			t.mchunks = append(t.mchunks, make([]q8meta, arenaChunkSlots))
		}
	}
	vc := t.schema.VecCol
	reg := t.region(si)
	copy(reg, full[vc].V)
	if t.useQ8() {
		t.mchunks[si>>arenaChunkShift][si&(arenaChunkSlots-1)] = encodeQ8(t.region8(si), reg)
	}
	if cache == nil {
		return nil
	}
	r := make(Row, len(cache))
	copy(r, cache)
	r[vc].V = reg
	return r
}
