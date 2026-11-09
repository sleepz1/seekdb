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

func Open(dir string) (*Engine, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	e := &Engine{dir: dir, state: State{}}
	b, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err == nil {
		if e.state, err = decodeState(b); err != nil {
			return nil, fmt.Errorf("recover snapshot: %w", err)
		}
		if e.state == nil {
			return nil, errors.New("invalid null snapshot")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var offset int64
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr == io.EOF {
			// Remove a torn tail before appending, including valid JSON lacking its
			// commit delimiter. Never silently skip a corrupt complete record.
			if err = f.Truncate(offset); err != nil {
				return nil, err
			}
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		var rec walRecord
		if err = json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("recover WAL at %d: %w", offset, err)
		}
		if rec.LSN != e.version+1 || len(rec.Statements) == 0 {
			return nil, fmt.Errorf("invalid WAL record at %d", offset)
		}
		for _, stmt := range rec.Statements {
			_, write, err := run(e.state, stmt)
			if err != nil || !write {
				return nil, fmt.Errorf("invalid WAL redo at %d: %s", offset, stmt)
			}
		}
		e.version = rec.LSN
		offset += int64(len(line))
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		return nil, err
	}
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
	if e.walErr != nil {
		return fmt.Errorf("WAL unavailable; restart required: %w", e.walErr)
	}
	if len(statements) == 0 {
		return nil
	}
	b, err := json.Marshal(walRecord{LSN: e.version + 1, Statements: statements})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(e.dir, "wal.log"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	b = append(b, '\n')
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	// A failed write/sync has an uncertain outcome. Reject further writes so a
	// partial record cannot be followed by another commit in this process.
	if err != nil {
		e.walErr = err
	}
	return err
}
