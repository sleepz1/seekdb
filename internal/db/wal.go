package db

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// A complete newline-terminated record is a committed transaction. Statements
// are validated before logging and replayed in order under the engine mutex.
// No active transaction is logged. CRC32 IEEE covers the semantic payload:
// little-endian uint64 LSN, then uint64 byte length + UTF-8 bytes per statement.
type walRecord struct {
	Checksum   uint32   `json:"checksum"`
	LSN        uint64   `json:"lsn"`
	Statements []string `json:"statements"`
}

func checksumUint(h hash.Hash32, n uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	h.Write(b[:])
}
func checksumStatement(h hash.Hash32, stmt string) {
	checksumUint(h, uint64(len(stmt)))
	io.WriteString(h, stmt)
}

func Open(dir string) (*Engine, error) { return OpenWithOptions(dir, OpenOptions{}) }

func OpenWithOptions(dir string, opts OpenOptions) (engine *Engine, openErr error) {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(storageError); ok {
				engine = nil
				openErr = e.error
			} else {
				panic(v)
			}
		}
	}()
	if opts.MemoryBudgetBytes < 0 {
		return nil, fmt.Errorf("negative memory budget")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	e := &Engine{dir: dir, state: State{}}
	var err error
	if opts.MemoryBudgetBytes > 0 {
		e.pages, err = newPageStore(dir, opts.MemoryBudgetBytes)
		if err != nil {
			return nil, err
		}
	}
	if err = e.loadSnapshot(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var offset, length int64
	for {
		chunk, readErr := r.ReadSlice('\n')
		length += int64(len(chunk))
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr == io.EOF {
			if err = f.Truncate(offset); err != nil {
				return nil, err
			}
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if err = e.replayRecord(io.NewSectionReader(f, offset, length)); err != nil {
			return nil, fmt.Errorf("recover WAL at %d: %w", offset, err)
		}
		offset += length
		length = 0
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		return nil, err
	}
	// Finish derived segment maintenance before opening the readiness gate.
	if e.pages != nil {
		if err = e.compactLocked(); err != nil {
			return nil, err
		}
		// One Engine owns this directory. Only after successful replay and a
		// synced replacement may abandoned generations and spools be removed.
		for _, pattern := range []string{"rows-*.page", "tx-*.pending"} {
			paths, err := filepath.Glob(filepath.Join(dir, "segments", pattern))
			if err != nil {
				return nil, err
			}
			for _, path := range paths {
				if path != e.pages.path {
					if err = os.Remove(path); err != nil {
						return nil, err
					}
				}
			}
		}
		if err = syncDir(filepath.Join(dir, "segments")); err != nil {
			return nil, err
		}
	}
	if opts.AfterRecoverBeforeRebuild != nil {
		opts.AfterRecoverBeforeRebuild(e)
	}
	for _, t := range e.state {
		rebuildIndexes(t)
	}
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
	return e, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (e *Engine) persist(statements []string) error {
	if len(statements) == 0 {
		return nil
	}
	return e.persistBatch(func(emit func(string) error) error {
		for _, stmt := range statements {
			if err := emit(stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

// Stream one JSON record, retaining the existing newline commit delimiter.
// Any failed write/sync poisons the writer until recovery truncates a torn tail.
func (e *Engine) persistBatch(each func(func(string) error) error) error {
	if e.walErr != nil {
		return fmt.Errorf("WAL unavailable; restart required: %w", e.walErr)
	}
	if e.pages != nil {
		if err := e.pages.sync(); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(e.dir, "wal.log"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	checksumUint(h, e.version+1)
	w := bufio.NewWriter(f)
	_, err = fmt.Fprintf(w, `{"lsn":%d,"statements":[`, e.version+1)
	if err == nil {
		first := true
		err = each(func(stmt string) error {
			checksumStatement(h, stmt)
			if !first {
				if _, err := w.WriteString(","); err != nil {
					return err
				}
			}
			first = false
			b, err := json.Marshal(stmt)
			if err != nil {
				return err
			}
			_, err = w.Write(b)
			return err
		})
	}
	if err == nil {
		_, err = fmt.Fprintf(w, "],\"checksum\":%d}\n", h.Sum32())
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		e.walErr = err
	}
	return err
}

// Verify before applying, with two streaming passes to bound transaction memory.
func (e *Engine) replayRecord(r *io.SectionReader) error {
	if err := e.readRecord(r, false); err != nil {
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return e.readRecord(r, true)
}
func (e *Engine) readRecord(r io.Reader, apply bool) error {
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return fmt.Errorf("invalid WAL object")
	}
	var lsn uint64
	var checksum uint32
	h := crc32.NewIEEE()
	count := 0
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		if seen[key] {
			return fmt.Errorf("duplicate WAL field")
		}
		seen[key] = true
		switch key {
		case "checksum":
			if err = d.Decode(&checksum); err != nil {
				return err
			}
		case "lsn":
			if err = d.Decode(&lsn); err != nil {
				return err
			}
		case "statements":
			if !seen["lsn"] {
				return fmt.Errorf("WAL LSN must precede statements")
			}
			checksumUint(h, lsn)
			tok, err = d.Token()
			if err != nil || tok != json.Delim('[') {
				return fmt.Errorf("invalid WAL statements")
			}
			for d.More() {
				var stmt string
				if err = d.Decode(&stmt); err != nil {
					return err
				}
				checksumStatement(h, stmt)
				if apply && lsn > e.snapshotLSN {
					_, write, err := runStoredMaintenance(e.state, stmt, false, e.pages, false)
					if err != nil {
						return err
					}
					if !write {
						return fmt.Errorf("non-write WAL statement")
					}

				}
				count++
			}
			if _, err = d.Token(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown WAL field")
		}
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing WAL data")
	}
	if (lsn > e.snapshotLSN && lsn != e.version+1) || lsn == 0 || count == 0 {
		return fmt.Errorf("invalid WAL LSN or empty batch")
	}
	if !seen["checksum"] || checksum != h.Sum32() {
		return fmt.Errorf("WAL checksum mismatch or missing checksum")
	}
	if apply && lsn > e.snapshotLSN {
		e.version = lsn
	}
	return nil
}
