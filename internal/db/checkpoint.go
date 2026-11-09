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

// Checkpoint durably publishes committed state before clearing WAL history.
// Automatic scheduling uses the same locked implementation as explicit callers.
func (e *Engine) Checkpoint() (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkpointLocked()
}

func (e *Engine) checkpointLocked() (err error) {
	defer func() {
		if v := recover(); v != nil {
			if se, ok := v.(storageError); ok {
				err = se.error
			} else {
				panic(v)
			}
		}
	}()
	if !e.ready {
		return fmt.Errorf("database not ready")
	}
	if e.activeTx != 0 {
		return fmt.Errorf("checkpoint requires no active transactions")
	}
	if e.walErr != nil {
		return fmt.Errorf("WAL unavailable; restart required: %w", e.walErr)
	}
	f, err := os.CreateTemp(e.dir, "snapshot-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	w := bufio.NewWriter(f)
	// Array envelope distinguishes checkpoints from legacy table-map snapshots.
	if _, err = fmt.Fprintf(w, "[%d,", e.version); err != nil {
		return err
	}
	if err = e.writeSnapshot(w); err != nil {
		return err
	}
	if _, err = io.WriteString(w, "]\n"); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(e.dir, "snapshot.json")); err != nil {
		return err
	}
	// Never remove redo until the snapshot's directory entry is durable.
	if err = syncDir(e.dir); err != nil {
		return err
	}
	e.snapshotLSN = e.version
	wal, err := os.OpenFile(filepath.Join(e.dir, "wal.log"), os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer wal.Close()
	if err = wal.Truncate(0); err == nil {
		err = wal.Sync()
	}
	if err != nil {
		e.walErr = err
	}
	if err == nil {
		e.checkpointVersion = e.version
		e.walBytes = 0
	}
	return err
}

// Defaults bound redo between statements, except while transactions pin state.
const AutoCheckpointWALBytes int64 = 8 << 20
const AutoCheckpointEveryN uint64 = 1024

func (e *Engine) maybeCheckpoint() {
	if !e.ready || e.activeTx != 0 || e.walErr != nil {
		return
	}
	if e.walBytes < AutoCheckpointWALBytes && e.version-e.checkpointVersion < AutoCheckpointEveryN {
		return
	}
	if err := e.checkpointLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "minivectordb: automatic checkpoint failed: %v\n", err)
	}
}

func (e *Engine) writeSnapshot(w io.Writer) error {
	enc := json.NewEncoder(w)
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	first := true
	for name, table := range e.state {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		if err := enc.Encode(name); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `:{"Columns":`); err != nil {
			return err
		}
		if err := enc.Encode(table.Columns); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"Indexes":`); err != nil {
			return err
		}
		if err := enc.Encode(table.Indexes); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"Rows":[`); err != nil {
			return err
		}
		for i := 0; i < table.rowCount(); i++ {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := enc.Encode(table.rowAt(i)); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "]}"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}

func (e *Engine) loadSnapshot() error {
	f, err := os.Open(filepath.Join(e.dir, "snapshot.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	// Peek past whitespace without loading the snapshot into memory.
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			r.UnreadByte()
			break
		}
	}
	b, _ := r.Peek(1)
	envelope := b[0] == '['
	d := json.NewDecoder(r)
	d.UseNumber()
	if envelope {
		if _, err = d.Token(); err != nil {
			return err
		}
		if err = d.Decode(&e.snapshotLSN); err != nil {
			return err
		}
	}
	if e.pages != nil {
		e.state, err = decodeSnapshotTables(d, e.pages)
	} else {
		var raw json.RawMessage
		if err = d.Decode(&raw); err == nil {
			e.state, err = decodeState(raw)
		}
	}
	if err != nil {
		return fmt.Errorf("recover snapshot: %w", err)
	}
	if envelope {
		tok, err := d.Token()
		if err != nil || tok != json.Delim(']') {
			return fmt.Errorf("invalid checkpoint envelope")
		}
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing snapshot data")
	}
	e.version = e.snapshotLSN
	return nil
}
