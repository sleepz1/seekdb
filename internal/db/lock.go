package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Keep the inode in place: unlinking LOCK could let another opener lock a new
// inode while the original owner still holds its lock. The kernel releases the
// advisory lock on descriptor close or process death, including SIGKILL.
func acquireDirLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot lock data directory %q (already in use or locking unavailable): %w", dir, err)
	}
	return f, nil
}

// Close disables all sessions and releases directory ownership. Commits are
// already durable; uncommitted transactions are abandoned, without checkpointing.
// It is safe to call Close repeatedly.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ready = false
	if e.lockFile == nil {
		return nil
	}
	err := e.lockFile.Close()
	e.lockFile = nil
	return err
}
