package db

import (
	"container/list"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OpenOptions bounds only the decoded row page cache, not process RSS.
// Excluded: active/oversized uncached rows, SQL/parser and copy buffers, returned
// result sets, schema, page references, PK validation keys, index buckets and
// ordering keys. ResidentRowBytes and PeakResidentRowBytes meter cached rows only.
type OpenOptions struct {
	MemoryBudgetBytes         int64
	AfterRecoverBeforeRebuild func(*Engine)
}
type pageRef struct {
	Offset int64
	Length int
}
type cachedPage struct {
	ref   pageRef
	row   []any
	bytes int64
}
type pageStore struct {
	path                   string
	budget, resident, peak int64
	cache                  map[pageRef]*list.Element
	lru                    *list.List
}
type storageError struct{ error }

func newPageStore(dir string, budget int64) (*pageStore, error) {
	dir = filepath.Join(dir, "segments")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	// Each open replays the authoritative WAL into a fresh segment. Old segments
	// are retained: an interrupted recovery must never destroy another generation.
	f, err := os.CreateTemp(dir, "rows-*.page")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	err = f.Close()
	return &pageStore{path: path, budget: budget, cache: map[pageRef]*list.Element{}, lru: list.New()}, err
}
func rowBytes(row []any) int64 {
	n := int64(24 + 16*len(row))
	for _, v := range row {
		switch x := v.(type) {
		case string:
			n += 16 + int64(len(x))
		case []float32:
			n += 24 + int64(4*len(x))
		default:
			n += 8
		}
	}
	return n
}
func (p *pageStore) put(row []any) pageRef {
	b, err := json.Marshal(row)
	if err != nil {
		panic(storageError{err})
	}
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(storageError{err})
	}
	off, err := f.Seek(0, 2)
	if err == nil {
		var n int
		n, err = f.Write(b)
		if err == nil && n != len(b) {
			err = fmt.Errorf("short page write")
		}
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		panic(storageError{err})
	}
	return pageRef{off, len(b)}
}
func (p *pageStore) get(ref pageRef, columns []Column) []any {
	if el := p.cache[ref]; el != nil {
		p.lru.MoveToFront(el)
		return el.Value.(cachedPage).row
	}
	// Evict before decoding. One oversized row is streamed uncached; it is the
	// minimum execution unit and never becomes resident cache state.
	for p.lru.Len() > 0 {
		p.evict()
	}
	b := make([]byte, ref.Length)
	f, err := os.Open(p.path)
	if err != nil {
		panic(storageError{err})
	}
	_, err = f.ReadAt(b, ref.Offset)
	f.Close()
	if err != nil {
		panic(storageError{err})
	}
	var raw []json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		panic(storageError{err})
	}
	if len(raw) != len(columns) {
		panic(storageError{fmt.Errorf("page column count mismatch")})
	}
	row := make([]any, len(raw))
	for i, c := range columns {
		literal := string(raw[i])
		if c.Type == "TEXT" || c.Type == "VARCHAR" {
			var s string
			if err = json.Unmarshal(raw[i], &s); err != nil {
				panic(storageError{err})
			}
			row[i], err = parseValue(c, "'"+strings.ReplaceAll(s, "'", "''")+"'")
			if err != nil {
				panic(storageError{err})
			}
			continue
		}
		row[i], err = parseValue(c, literal)
		if err != nil {
			panic(storageError{err})
		}
	}
	n := rowBytes(row)
	if n <= p.budget {
		el := p.lru.PushFront(cachedPage{ref, row, n})
		p.cache[ref] = el
		p.resident += n
		if p.resident > p.peak {
			p.peak = p.resident
		}
	}
	return row
}
func (p *pageStore) evict() {
	el := p.lru.Back()
	c := el.Value.(cachedPage)
	delete(p.cache, c.ref)
	p.resident -= c.bytes
	p.lru.Remove(el)
}
func (p *pageStore) sync() error {
	f, err := os.OpenFile(p.path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p.path))
}
func (t *Table) rowCount() int {
	if t.pages != nil {
		return len(t.refs)
	}
	return len(t.Rows)
}
func (t *Table) rowAt(i int) []any {
	if t.pages != nil {
		return t.pages.get(t.refs[i], t.Columns)
	}
	return t.Rows[i]
}
func (t *Table) appendRow(row []any) {
	if t.pages != nil {
		t.refs = append(t.refs, t.pages.put(row))
	} else {
		t.Rows = append(t.Rows, row)
	}
}
func (t *Table) replaceRow(i int, row []any) {
	if t.pages != nil {
		t.refs[i] = t.pages.put(row)
	} else {
		t.Rows[i] = row
	}
}
func (e *Engine) Ready() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.ready }
func (e *Engine) ResidentRowBytes() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pages != nil {
		return e.pages.resident
	}
	var n int64
	for _, t := range e.state {
		for _, r := range t.Rows {
			n += rowBytes(r)
		}
	}
	return n
}
func (e *Engine) PeakResidentRowBytes() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pages != nil {
		return e.pages.peak
	}
	return 0
}
func (e *Engine) ResidentPages() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pages != nil {
		return e.pages.lru.Len()
	}
	return 0
}

// Stream even legacy snapshots one row at a time. Column order in JSON does not
// matter: raw rows are first spooled and then validated against the schema.
func decodePagedSnapshot(r io.Reader, p *pageStore) (State, error) {
	d := json.NewDecoder(r)
	d.UseNumber()
	state, err := decodeSnapshotTables(d, p)
	if err != nil {
		return nil, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing snapshot data")
	}
	return state, nil
}

func decodeSnapshotTables(d *json.Decoder, p *pageStore) (State, error) {
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid snapshot object")
	}
	state := State{}
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return nil, err
		}
		tok, err := d.Token()
		if err != nil || tok != json.Delim('{') {
			return nil, fmt.Errorf("invalid snapshot table")
		}
		t := &Table{pages: p}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch strings.ToLower(key.(string)) {
			case "columns":
				err = d.Decode(&t.Columns)
			case "indexes":
				err = d.Decode(&t.Indexes)
			case "rows":
				tok, err = d.Token()
				if err != nil {
					return nil, err
				}
				if tok == nil {
					continue
				}
				if tok != json.Delim('[') {
					return nil, fmt.Errorf("invalid snapshot rows")
				}
				for d.More() {
					var row []any
					if err = d.Decode(&row); err != nil {
						return nil, err
					}
					t.appendRow(row)
				}
				_, err = d.Token()
			default:
				var discard json.RawMessage
				err = d.Decode(&discard)
			}
			if err != nil {
				return nil, err
			}
		}
		if _, err = d.Token(); err != nil {
			return nil, err
		}
		for i := 0; i < t.rowCount(); i++ {
			t.rowAt(i)
		}
		state[name.(string)] = t
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	return state, nil
}
