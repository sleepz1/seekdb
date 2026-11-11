// Package engine implements MiniVectorDB storage, transactions and execution.
package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"minivectordb/internal/sqlparse"
)

// Log payload op codes.
const (
	opCreateTable byte = 1
	opDropTable   byte = 2
	opCreateIndex byte = 3
	opDropIndex   byte = 4
	opPut         byte = 5
	opDelete      byte = 6
)

// slot locates the committed version of one row.
type slot struct {
	off  int64  // file offset of the encoded row
	pk   int64  // primary key
	n    uint32 // encoded length
	used bool   // false for free slots
	row  Row    // cached decoded row, nil when not cached
	crow []byte // compact cached row (arena tables): non-vector columns, encoded
}

// cached reports whether the slot's row is held in memory (decoded or compact).
func (s *slot) cached() bool { return s.row != nil || s.crow != nil }

// cacheSize is what the slot's cached row counts against the cache budget.
func (s *slot) cacheSize() int64 {
	if s.row != nil {
		return s.row.approxSize()
	}
	if s.crow != nil {
		return int64(len(s.crow)) + 32
	}
	return 0
}

// Table is the committed state of a table.
type Table struct {
	schema   *Schema
	index    map[int64]int32 // primary key -> slot number
	slots    []slot
	free     []int32
	vecIndex string // vector index name, "" if none
	live     int64  // encoded bytes of live rows
	uncached int    // used slots whose row is not cached
	version  uint64 // bumped by every committed change (invalidates postings)
	postings map[int]*postingList

	// vector arena (see arena.go)
	vdim    int
	vchunks [][]float32
	qchunks [][]int8 // int8 copy of the vectors (pruning only, see q8.go)
	// per-slot metadata of the int8 copy, one array per field so that the
	// prune kernel can load four rows at a time
	msc      [][]float32 // scale
	mc2      [][]int32   // sum of c^2
	mre      [][]float32 // residual bound
	arenaOff bool
}

func newTable(s *Schema) *Table {
	t := &Table{schema: s, index: make(map[int64]int32)}
	if s.VecCol >= 0 {
		t.vdim = s.Columns[s.VecCol].Param
	}
	return t
}

// Options configures an Engine.
type Options struct {
	Dir         string
	CacheBudget int64 // bytes of decoded rows kept in memory; <=0 = auto
	// CompactMinBytes is the log size below which compaction never runs.
	CompactMinBytes int64
}

// Engine owns the data directory.
type Engine struct {
	opts      Options
	log       *logFile
	tables    map[string]*Table // lower-cased name
	cacheUsed int64
	arenaUsed int64
	budget    int64
}

// Open recovers the database in dir. The returned engine is ready only after
// every committed record has been replayed and the cache rebuilt.
func Open(opts Options) (*Engine, error) {
	if opts.Dir == "" {
		opts.Dir = "/data"
	}
	if opts.CompactMinBytes <= 0 {
		opts.CompactMinBytes = 64 << 20
	}
	e := &Engine{opts: opts, tables: make(map[string]*Table)}
	e.budget = opts.CacheBudget
	if e.budget <= 0 {
		e.budget = autoCacheBudget()
	}
	lf, err := openLog(opts.Dir, e.replay)
	if err != nil {
		return nil, err
	}
	e.log = lf
	return e, nil
}

// autoCacheBudget derives a row-cache budget from the cgroup memory limit and
// sets a soft Go heap limit so GC tightens before the container limit.
func autoCacheBudget() int64 {
	limit := cgroupMemoryLimit()
	if limit <= 0 {
		return 512 << 20
	}
	debug.SetMemoryLimit(limit * 6 / 10)
	b := limit / 4
	if b < 8<<20 {
		b = 8 << 20
	}
	return b
}

func cgroupMemoryLimit() int64 {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 || v > 1<<50 {
			return 0
		}
		return v
	}
	return 0
}

// Close releases the log file. Committed data is already durable.
func (e *Engine) Close() error { return e.log.close() }

func (e *Engine) table(name string) (*Table, error) {
	t := e.tables[strings.ToLower(name)]
	if t == nil {
		return nil, newErr(ErrNoTable, "Table '%s' doesn't exist", name)
	}
	return t, nil
}

// ---- payload encoding -------------------------------------------------

type payloadBuf struct{ b []byte }

func (p *payloadBuf) byte(v byte) { p.b = append(p.b, v) }
func (p *payloadBuf) str(s string) {
	p.b = binary.AppendUvarint(p.b, uint64(len(s)))
	p.b = append(p.b, s...)
}
func (p *payloadBuf) uvar(v uint64) { p.b = binary.AppendUvarint(p.b, v) }
func (p *payloadBuf) i64(v int64)   { p.b = binary.LittleEndian.AppendUint64(p.b, uint64(v)) }

type payloadReader struct {
	b   []byte
	pos int
	err error
}

func (r *payloadReader) byte() byte {
	if r.err != nil || r.pos >= len(r.b) {
		r.err = errCorrupt
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *payloadReader) uvar() uint64 {
	if r.err != nil {
		return 0
	}
	v, k := binary.Uvarint(r.b[r.pos:])
	if k <= 0 {
		r.err = errCorrupt
		return 0
	}
	r.pos += k
	return v
}

func (r *payloadReader) str() string {
	n := int(r.uvar())
	if r.err != nil || r.pos+n > len(r.b) {
		r.err = errCorrupt
		return ""
	}
	s := string(r.b[r.pos : r.pos+n])
	r.pos += n
	return s
}

func (r *payloadReader) i64() int64 {
	if r.err != nil || r.pos+8 > len(r.b) {
		r.err = errCorrupt
		return 0
	}
	v := int64(binary.LittleEndian.Uint64(r.b[r.pos:]))
	r.pos += 8
	return v
}

func encodeSchema(p *payloadBuf, s *Schema) {
	p.byte(opCreateTable)
	p.str(s.Name)
	p.uvar(uint64(len(s.Columns)))
	for _, c := range s.Columns {
		p.str(c.Name)
		p.byte(byte(c.Type))
		p.uvar(uint64(c.Param))
	}
	p.uvar(uint64(s.PK))
}

// ---- recovery ---------------------------------------------------------

// replay applies one committed record during recovery.
func (e *Engine) replay(payload []byte, payloadOff int64) error {
	r := &payloadReader{b: payload}
	for r.pos < len(payload) && r.err == nil {
		op := r.byte()
		switch op {
		case opCreateTable:
			s := &Schema{Name: r.str(), VecCol: -1}
			n := int(r.uvar())
			for i := 0; i < n && r.err == nil; i++ {
				c := Column{Name: r.str(), Type: sqlparse.ColType(r.byte()), Param: int(r.uvar())}
				if c.Type == sqlparse.TypeVector {
					s.VecCol = i
				}
				s.Columns = append(s.Columns, c)
			}
			s.PK = int(r.uvar())
			if r.err == nil {
				e.tables[strings.ToLower(s.Name)] = newTable(s)
			}
		case opDropTable:
			name := strings.ToLower(r.str())
			if t := e.tables[name]; t != nil {
				e.dropCache(t)
				delete(e.tables, name)
			}
		case opCreateIndex:
			idx, tname := r.str(), r.str()
			if t := e.tables[strings.ToLower(tname)]; t != nil {
				t.vecIndex = idx
			}
		case opDropIndex:
			_, tname := r.str(), r.str()
			if t := e.tables[strings.ToLower(tname)]; t != nil {
				t.vecIndex = ""
			}
		case opPut:
			tname, pk := r.str(), r.i64()
			n := int(r.uvar())
			if r.err != nil || r.pos+n > len(payload) {
				return fmt.Errorf("recovery: corrupt put record at offset %d", payloadOff)
			}
			t := e.tables[strings.ToLower(tname)]
			if t == nil {
				return fmt.Errorf("recovery: put into unknown table %q", tname)
			}
			var row Row
			if e.cacheUsed < e.budget || t.arenaOn() {
				var err error
				if row, err = decodeRow(t.schema, payload[r.pos:r.pos+n]); err != nil {
					return fmt.Errorf("recovery: %w", err)
				}
			}
			e.putSlot(t, pk, payloadOff+int64(r.pos), uint32(n), row)
			r.pos += n
		case opDelete:
			tname, pk := r.str(), r.i64()
			if t := e.tables[strings.ToLower(tname)]; t != nil {
				e.deleteSlot(t, pk)
			}
		default:
			return fmt.Errorf("recovery: unknown op %d at offset %d", op, payloadOff)
		}
	}
	if r.err != nil {
		return fmt.Errorf("recovery: corrupt record at offset %d", payloadOff)
	}
	return nil
}

// ---- committed state maintenance ---------------------------------------

func (e *Engine) putSlot(t *Table, pk int64, off int64, n uint32, row Row) {
	t.version++
	si, ok := t.index[pk]
	if ok {
		s := &t.slots[si]
		if s.cached() {
			e.cacheUsed -= s.cacheSize()
		} else {
			t.uncached--
		}
		t.live -= int64(s.n)
	} else if k := len(t.free); k > 0 {
		si = t.free[k-1]
		t.free = t.free[:k-1]
	} else {
		si = int32(len(t.slots))
		t.slots = append(t.slots, slot{})
	}
	ns := slot{off: off, pk: pk, n: n, used: true}
	t.slots[si] = ns // the arena may be disabled below and must see a consistent slot
	if row != nil && e.arenaStore(t, si, row) {
		// Arena tables cache only the non-vector columns, compactly; the
		// vector is served from the arena (see cachedRow).
		c := encodeCompact(t.schema, row)
		if sz := int64(len(c)) + 32; e.cacheUsed+sz <= e.budget {
			ns.crow = c
			e.cacheUsed += sz
		}
	} else if row != nil {
		if sz := row.approxSize(); e.cacheUsed+sz <= e.budget {
			ns.row = row
			e.cacheUsed += sz
		}
	}
	if !ns.cached() {
		t.uncached++
	}
	t.slots[si] = ns
	t.index[pk] = si
	t.live += int64(n)
}

func (e *Engine) deleteSlot(t *Table, pk int64) {
	si, ok := t.index[pk]
	if !ok {
		return
	}
	t.version++
	s := &t.slots[si]
	if s.cached() {
		e.cacheUsed -= s.cacheSize()
	} else {
		t.uncached--
	}
	t.live -= int64(s.n)
	*s = slot{}
	t.free = append(t.free, si)
	delete(t.index, pk)
}

func (e *Engine) dropCache(t *Table) {
	e.disableArena(t)
	for i := range t.slots {
		e.cacheUsed -= t.slots[i].cacheSize()
	}
}

// cachedRow returns the in-memory committed row of slot i, or nil when the
// row is not cached. Compact rows are decoded on demand; their vector value
// aliases the arena region of the slot.
func (t *Table) cachedRow(i int) Row {
	sl := &t.slots[i]
	if sl.row != nil {
		return sl.row
	}
	if sl.crow != nil {
		r, err := decodeCompact(t.schema, sl.crow, t.region(int32(i)))
		if err == nil {
			return r
		}
	}
	return nil
}

// loadRow returns the committed row stored in slot si.
func (e *Engine) loadRow(t *Table, si int32) (Row, error) {
	s := &t.slots[si]
	if r := t.cachedRow(int(si)); r != nil {
		return r, nil
	}
	b, err := e.log.readAt(s.off, s.n)
	if err != nil {
		return nil, err
	}
	return decodeRow(t.schema, b)
}

// scanCommitted calls fn for each committed row of t. Uncached rows are read
// in file order to keep disk access sequential.
func (e *Engine) scanCommitted(t *Table, skip func(pk int64) bool, fn func(pk int64, r Row) error) error {
	type ref struct {
		pk int64
		si int32
	}
	var disk []ref
	// Walk the slot array linearly: sequential memory access is much faster
	// than map iteration for full scans.
	for i := range t.slots {
		sl := &t.slots[i]
		if !sl.used {
			continue
		}
		if skip != nil && skip(sl.pk) {
			continue
		}
		if r := t.cachedRow(i); r != nil {
			if err := fn(sl.pk, r); err != nil {
				return err
			}
			continue
		}
		disk = append(disk, ref{sl.pk, int32(i)})
	}
	if len(disk) == 0 {
		return nil
	}
	sort.Slice(disk, func(i, j int) bool { return t.slots[disk[i].si].off < t.slots[disk[j].si].off })
	for _, d := range disk {
		r, err := e.loadRow(t, d.si)
		if err != nil {
			return err
		}
		if err := fn(d.pk, r); err != nil {
			return err
		}
	}
	return nil
}

// change is one pending row modification.
type change struct {
	row     Row
	deleted bool
}

// commitChanges durably writes the given per-table changes as one record and
// then publishes them to the committed state.
func (e *Engine) commitChanges(changes map[string]map[int64]change) error {
	names := make([]string, 0, len(changes))
	for n, m := range changes {
		if len(m) > 0 {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	type pending struct {
		t   *Table
		pk  int64
		pos int // payload position of the encoded row
		n   int
		row Row
	}
	var pend []pending
	var dels []pending
	p := &payloadBuf{}
	for _, name := range names {
		t := e.tables[name]
		if t == nil {
			return newErr(ErrNoTable, "Table '%s' doesn't exist", name)
		}
		m := changes[name]
		pks := make([]int64, 0, len(m))
		for pk := range m {
			pks = append(pks, pk)
		}
		sort.Slice(pks, func(i, j int) bool { return pks[i] < pks[j] })
		// Encode the rows (pure work) in parallel for large commits.
		encs := make([][]byte, len(pks))
		encode := func(lo, hi int) {
			for i := lo; i < hi; i++ {
				if c := m[pks[i]]; !c.deleted {
					encs[i] = encodeRow(nil, t.schema, c.row)
				}
			}
		}
		if Workers > 1 && len(pks) >= 32 {
			parallelFor(len(pks), encode)
		} else {
			encode(0, len(pks))
		}
		for pi, pk := range pks {
			c := m[pk]
			if c.deleted {
				if _, ok := t.index[pk]; !ok {
					continue // insert+delete inside one transaction
				}
				p.byte(opDelete)
				p.str(t.schema.Name)
				p.i64(pk)
				dels = append(dels, pending{t: t, pk: pk})
				continue
			}
			p.byte(opPut)
			p.str(t.schema.Name)
			p.i64(pk)
			enc := encs[pi]
			p.uvar(uint64(len(enc)))
			pos := len(p.b)
			p.b = append(p.b, enc...)
			pend = append(pend, pending{t: t, pk: pk, pos: pos, n: len(enc), row: c.row})
		}
	}
	if len(p.b) == 0 {
		return nil
	}
	base, err := e.log.appendRecord(p.b)
	if err != nil {
		return newErr(ErrInternal, "commit failed: %v", err)
	}
	for _, d := range dels {
		e.deleteSlot(d.t, d.pk)
	}
	for _, w := range pend {
		e.putSlot(w.t, w.pk, base+int64(w.pos), uint32(w.n), w.row)
	}
	e.maybeCompact()
	return nil
}

// commitDDL durably writes a single DDL payload.
func (e *Engine) commitDDL(payload []byte) error {
	if _, err := e.log.appendRecord(payload); err != nil {
		return newErr(ErrInternal, "DDL failed: %v", err)
	}
	return nil
}

// ---- compaction -------------------------------------------------------

func (e *Engine) liveBytes() int64 {
	var n int64
	for _, t := range e.tables {
		n += t.live + int64(len(t.index))*int64(16+len(t.schema.Name))
	}
	return n
}

func (e *Engine) maybeCompact() {
	size := e.log.size
	if size < e.opts.CompactMinBytes {
		return
	}
	if size < 2*e.liveBytes() {
		return
	}
	if err := e.Compact(); err != nil {
		fmt.Fprintf(os.Stderr, "minivectordb: compaction skipped: %v\n", err)
	}
}

// Compact rewrites the log with only live rows and atomically replaces it.
// A kill at any point leaves either the old or the new complete log.
func (e *Engine) Compact() error {
	lw, err := newLogWriter(e.opts.Dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(e.tables))
	for n := range e.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	ddl := &payloadBuf{}
	for _, n := range names {
		t := e.tables[n]
		encodeSchema(ddl, t.schema)
		if t.vecIndex != "" {
			ddl.byte(opCreateIndex)
			ddl.str(t.vecIndex)
			ddl.str(t.schema.Name)
		}
	}
	if len(ddl.b) > 0 {
		if _, err := lw.write(ddl.b); err != nil {
			lw.abort(e.opts.Dir)
			return err
		}
	}
	type moved struct {
		t   *Table
		si  int32
		off int64
	}
	var moves []moved
	for _, n := range names {
		t := e.tables[n]
		pks := make([]int64, 0, len(t.index))
		for pk := range t.index {
			pks = append(pks, pk)
		}
		sort.Slice(pks, func(i, j int) bool { return t.slots[t.index[pks[i]]].off < t.slots[t.index[pks[j]]].off })
		p := &payloadBuf{}
		var chunk []moved
		var chunkPos []int
		flush := func() error {
			if len(p.b) == 0 {
				return nil
			}
			base, err := lw.write(p.b)
			if err != nil {
				return err
			}
			for i := range chunk {
				chunk[i].off = base + int64(chunkPos[i])
			}
			moves = append(moves, chunk...)
			p.b = p.b[:0]
			chunk, chunkPos = nil, nil
			return nil
		}
		for _, pk := range pks {
			si := t.index[pk]
			s := &t.slots[si]
			enc, err := e.log.readAt(s.off, s.n)
			if err != nil {
				lw.abort(e.opts.Dir)
				return err
			}
			p.byte(opPut)
			p.str(t.schema.Name)
			p.i64(pk)
			p.uvar(uint64(len(enc)))
			chunkPos = append(chunkPos, len(p.b))
			chunk = append(chunk, moved{t: t, si: si})
			p.b = append(p.b, enc...)
			if len(p.b) >= 4<<20 {
				if err := flush(); err != nil {
					lw.abort(e.opts.Dir)
					return err
				}
			}
		}
		if err := flush(); err != nil {
			lw.abort(e.opts.Dir)
			return err
		}
	}
	if err := lw.install(e.log); err != nil {
		return err
	}
	for _, m := range moves {
		m.t.slots[m.si].off = m.off
	}
	return nil
}

var errStop = errors.New("stop")
