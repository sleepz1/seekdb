package db

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A complete newline-terminated record is a committed transaction. Statements
// are validated before logging and replayed in order under the engine mutex.
// No active transaction is logged. The snapshot is an immutable legacy base;
// WAL compaction/checkpointing is deliberately deferred.
type walRecord struct {
	LSN        uint64   `json:"lsn"`
	Statements []string `json:"statements"`
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
	if e.pages != nil {
		f, err := os.Open(filepath.Join(dir, "snapshot.json"))
		if err == nil {
			e.state, err = decodePagedSnapshot(f, e.pages)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("recover snapshot: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		b, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
		if err == nil {
			e.state, err = decodeState(b)
			if err != nil {
				return nil, fmt.Errorf("recover snapshot: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
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
	w := bufio.NewWriter(f)
	_, err = fmt.Fprintf(w, `{"lsn":%d,"statements":[`, e.version+1)
	if err == nil {
		first := true
		err = each(func(stmt string) error {
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
		_, err = w.WriteString("]}\n")
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

func (e *Engine) replayRecord(r io.Reader) error {
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return fmt.Errorf("invalid WAL object")
	}
	var lsn uint64
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
		case "lsn":
			if err = d.Decode(&lsn); err != nil {
				return err
			}
		case "statements":
			tok, err = d.Token()
			if err != nil || tok != json.Delim('[') {
				return fmt.Errorf("invalid WAL statements")
			}
			for d.More() {
				var stmt string
				if err = d.Decode(&stmt); err != nil {
					return err
				}
				_, write, err := runStored(e.state, stmt, false, e.pages)
				if err != nil {
					return err
				}
				if !write {
					return fmt.Errorf("non-write WAL statement")
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
	if lsn != e.version+1 || count == 0 {
		return fmt.Errorf("invalid WAL LSN or empty batch")
	}
	e.version = lsn
	return nil
}
