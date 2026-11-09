package db

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Compact rewrites live page references without changing row order or index
// offsets. Snapshot + WAL remain authoritative; segments are disposable derived
// state. Active transactions pin references, so explicit compaction rejects them.
// Unlimited Open has no page store and needs no segment compaction.
func (e *Engine) Compact() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		return fmt.Errorf("database not ready")
	}
	if e.activeTx != 0 {
		return fmt.Errorf("compaction requires no active transactions")
	}
	return e.compactLocked()
}

func (e *Engine) compactLocked() error {
	if e.pages == nil {
		return nil
	}
	old := e.pages
	src, err := os.Open(old.path)
	if err != nil {
		return err
	}
	defer src.Close()
	next, err := newPageStore(e.dir, old.budget)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.Remove(next.path)
		}
	}()
	dst, err := os.OpenFile(next.path, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer dst.Close()
	refs := make(map[*Table][]pageRef)
	var offset int64
	for _, table := range e.state {
		rr := make([]pageRef, 0, len(table.refs))
		for _, ref := range table.refs {
			n, err := io.CopyN(dst, io.NewSectionReader(src, ref.Offset, int64(ref.Length)), int64(ref.Length))
			if err != nil {
				return err
			}
			rr = append(rr, pageRef{offset, ref.Length})
			offset += n
		}
		refs[table] = rr
	}
	if err = dst.Sync(); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(next.path)); err != nil {
		return err
	}
	// All fallible copy/sync work precedes publication. Buckets address row
	// ordinals, which are unchanged, and therefore need no rebuild here.
	next.peak = old.peak
	for table, rr := range refs {
		table.refs, table.pages = rr, next
	}
	e.pages = next
	published = true
	if err = os.Remove(old.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(next.path))
}

// Maintenance failure must not turn an already durable COMMIT into an error.
// Explicit Compact exposes errors; failed automatic copies retain the old store.
func (e *Engine) maybeCompact() {
	if e.pages == nil || e.activeTx != 0 {
		return
	}
	info, err := os.Stat(e.pages.path)
	if err != nil || info.Size() < 1<<20 {
		return
	}
	var live int64
	for _, table := range e.state {
		for _, ref := range table.refs {
			live += int64(ref.Length)
		}
	}
	if info.Size() > 2*live {
		_ = e.compactLocked()
	}
}
