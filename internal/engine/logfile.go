package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// The commit log is the only durable structure. Layout:
//
//	file   := magic record*
//	record := u32 payloadLen | u32 crc32c(payload) | payload
//
// Every committed transaction (or autocommit statement, or DDL) is exactly one
// record, written to the file with one pwrite before success is reported. A
// record is therefore applied entirely or not at all. The crash model of the
// task is a killed process: once pwrite has returned, the record is in the
// kernel's page cache and survives the kill. A background goroutine then
// fdatasyncs the log right away (see bgSyncer); MVDB_SYNC_COMMIT=1 makes
// every commit wait for its own fdatasync instead. A process kill can only leave a
// torn record at the tail, which recovery detects (short read or CRC
// mismatch) and truncates. Truncation is idempotent, so a kill during
// recovery is harmless.

const (
	logName     = "mvdb.log"
	logTmpName  = "mvdb.log.compact"
	logMagic    = "MVDBLOG1"
	recHdrSize  = 8
	maxRecBytes = 1 << 30
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type logFile struct {
	dir    string
	f      *os.File
	size   int64
	failed error     // sticky I/O failure; no further durable writes after it
	bg     *bgSyncer // nil: every commit waits for its fdatasync
}

// syncEachCommit restores the strict mode: a commit is reported only after
// its own fdatasync.
var syncEachCommit = os.Getenv("MVDB_SYNC_COMMIT") == "1"

// bgSyncer makes the appended part of the log durable off the request path.
// It always syncs everything written so far, so one fdatasync can cover
// several commits, and then drops the synced pages from the page cache.
type bgSyncer struct {
	mu      sync.Mutex
	cond    sync.Cond
	f       *os.File
	want    int64 // bytes written to f
	done    int64 // bytes known durable
	busy    bool
	closing bool
	err     error // first fdatasync failure (sticky)
	exited  chan struct{}
}

func startSyncer(f *os.File, size int64) *bgSyncer {
	b := &bgSyncer{f: f, want: size, done: size, exited: make(chan struct{})}
	b.cond.L = &b.mu
	go b.run()
	return b
}

func (b *bgSyncer) run() {
	b.mu.Lock()
	for {
		for b.want <= b.done && !b.closing {
			b.cond.Wait()
		}
		if b.want <= b.done || b.err != nil {
			break // closing with nothing left, or failed
		}
		f, end := b.f, b.want
		b.busy = true
		b.mu.Unlock()
		err := fdatasync(f)
		if err == nil {
			dropPageCache(f, end)
		}
		b.mu.Lock()
		b.busy = false
		if err != nil {
			b.err = err
		} else if f == b.f && end > b.done {
			b.done = end
		}
		b.cond.Broadcast()
		if err != nil {
			break
		}
	}
	close(b.exited)
	b.mu.Unlock()
}

// wrote records that the file now holds size bytes and wakes the syncer.
func (b *bgSyncer) wrote(size int64) error {
	b.mu.Lock()
	err := b.err
	b.want = size
	b.cond.Signal()
	b.mu.Unlock()
	return err
}

// failure returns the sticky fdatasync error, if any.
func (b *bgSyncer) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// drain waits until everything written is durable (or syncing failed) and
// no fdatasync is in flight.
func (b *bgSyncer) drain() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.err == nil && (b.busy || b.want > b.done) {
		b.cond.Wait()
	}
	return b.err
}

// swap points the syncer at a new, already durable file of the given size.
// The caller has drained it.
func (b *bgSyncer) swap(f *os.File, size int64) {
	b.mu.Lock()
	b.f, b.want, b.done = f, size, size
	b.mu.Unlock()
}

// stop syncs what is left and ends the goroutine.
func (b *bgSyncer) stop() error {
	b.mu.Lock()
	b.closing = true
	b.cond.Broadcast()
	b.mu.Unlock()
	<-b.exited
	return b.failure()
}

// openLog opens (creating if needed) the log and streams every valid record
// to fn together with the absolute file offset of its payload.
func openLog(dir string, fn func(payload []byte, payloadOff int64) error) (*logFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// A leftover compaction file is an unfinished rewrite: the original log is
	// still authoritative.
	_ = os.Remove(filepath.Join(dir, logTmpName))

	path := filepath.Join(dir, logName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	lf := &logFile{dir: dir, f: f}
	if st.Size() < int64(len(logMagic)) {
		// New (or torn during creation) log: write the header durably.
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.WriteAt([]byte(logMagic), 0); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if err := syncDir(dir); err != nil {
			f.Close()
			return nil, err
		}
		lf.size = int64(len(logMagic))
		lf.startBg()
		return lf, nil
	}
	magic := make([]byte, len(logMagic))
	if _, err := f.ReadAt(magic, 0); err != nil || string(magic) != logMagic {
		f.Close()
		return nil, fmt.Errorf("%s: bad log header", path)
	}
	good, err := scanRecords(f, int64(len(logMagic)), fn)
	if err != nil {
		f.Close()
		return nil, err
	}
	if good < st.Size() {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
		if err := fdatasync(f); err != nil {
			f.Close()
			return nil, err
		}
	}
	lf.size = good
	dropPageCache(f, good)
	lf.startBg()
	return lf, nil
}

func (l *logFile) startBg() {
	if !syncEachCommit {
		l.bg = startSyncer(l.f, l.size)
	}
}

// scanRecords reads records from start and returns the end offset of the last
// complete valid record.
func scanRecords(f *os.File, start int64, fn func([]byte, int64) error) (int64, error) {
	// Small read window, no kernel readahead, and the page cache of what has
	// been replayed is dropped every 256 KiB: the log passes through memory
	// once, and none of it stays charged to the container while the tables
	// are being built.
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, 1 /* POSIX_FADV_RANDOM */, 0, 0)
	r := bufio.NewReaderSize(io.NewSectionReader(f, start, 1<<62), 64<<10)
	off := start
	var hdr [recHdrSize]byte
	var buf []byte
	dropped := off
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return off, nil // EOF or torn header
		}
		n := binary.LittleEndian.Uint32(hdr[0:])
		sum := binary.LittleEndian.Uint32(hdr[4:])
		if n == 0 || n > maxRecBytes {
			return off, nil
		}
		if cap(buf) < int(n) {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return off, nil // torn payload
		}
		if crc32.Checksum(buf, crcTable) != sum {
			return off, nil
		}
		if err := fn(buf, off+recHdrSize); err != nil {
			return 0, err
		}
		off += recHdrSize + int64(n)
		if off-dropped >= 256<<10 {
			dropPageCache(f, off-int64(r.Buffered()))
			dropped = off
		}
	}
}

// appendRecord writes one record and makes it durable. It returns the file
// offset of the payload.
func (l *logFile) appendRecord(payload []byte) (int64, error) {
	buf := make([]byte, recHdrSize+len(payload))
	copy(buf[recHdrSize:], payload)
	return l.appendFramed(buf)
}

// appendFramed is appendRecord for a buffer whose first recHdrSize bytes
// are reserved for the record header (the payload follows them), which
// saves copying a large commit payload.
func (l *logFile) appendFramed(buf []byte) (int64, error) {
	if l.failed == nil && l.bg != nil {
		if err := l.bg.failure(); err != nil {
			l.failed = err
		}
	}
	if l.failed != nil {
		return 0, fmt.Errorf("storage unavailable after earlier I/O failure: %w", l.failed)
	}
	payload := buf[recHdrSize:]
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(buf[4:], crc32.Checksum(payload, crcTable))
	start := l.size
	if _, err := l.f.WriteAt(buf, start); err != nil {
		l.fail(err, start)
		return 0, err
	}
	if l.bg != nil {
		l.size = start + int64(len(buf))
		_ = l.bg.wrote(l.size) // a sync failure surfaces on the next write
		return start + recHdrSize, nil
	}
	if err := fdatasync(l.f); err != nil {
		l.fail(err, start)
		return 0, err
	}
	l.size = start + int64(len(buf))
	dropPageCache(l.f, l.size)
	return start + recHdrSize, nil
}

// dropPageCache asks the kernel to evict the clean, fully written pages of
// the log below end. Committed data is already durable (fdatasync) and every
// row the engine needs is in memory or re-read with pread, so keeping the
// log in the page cache only inflates the container's memory footprint. The
// last, partial page is kept so the next append does not have to read it
// back. Best effort: errors are ignored.
func dropPageCache(f *os.File, end int64) {
	end &^= 4095
	if end <= 0 {
		return
	}
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, uintptr(end), 4 /* POSIX_FADV_DONTNEED */, 0, 0)
}

func (l *logFile) fail(err error, keep int64) {
	// Best effort: drop the partial record so a later restart sees a clean tail.
	_ = l.f.Truncate(keep)
	l.failed = err
}

func (l *logFile) readAt(off int64, n uint32) ([]byte, error) {
	b := make([]byte, n)
	if _, err := l.f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

func (l *logFile) close() error {
	if l.f == nil {
		return nil
	}
	var serr error
	if l.bg != nil {
		serr = l.bg.stop()
		l.bg = nil
	}
	err := l.f.Close()
	if err == nil {
		err = serr
	}
	l.f = nil
	return err
}

// logWriter builds a replacement log for compaction.
type logWriter struct {
	f   *os.File
	w   *bufio.Writer
	off int64
}

func newLogWriter(dir string) (*logWriter, error) {
	f, err := os.OpenFile(filepath.Join(dir, logTmpName), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.WriteString(logMagic); err != nil {
		f.Close()
		return nil, err
	}
	return &logWriter{f: f, w: w, off: int64(len(logMagic))}, nil
}

// write appends a record and returns its payload offset.
func (lw *logWriter) write(payload []byte) (int64, error) {
	var hdr [recHdrSize]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:], crc32.Checksum(payload, crcTable))
	if _, err := lw.w.Write(hdr[:]); err != nil {
		return 0, err
	}
	if _, err := lw.w.Write(payload); err != nil {
		return 0, err
	}
	start := lw.off + recHdrSize
	lw.off += recHdrSize + int64(len(payload))
	return start, nil
}

// install makes the rewritten log durable and atomically replaces the old one.
func (lw *logWriter) install(old *logFile) error {
	if old.bg != nil {
		// Nothing may still be syncing the old file when it is closed below.
		if err := old.bg.drain(); err != nil {
			lw.abort(old.dir)
			return err
		}
	}
	if err := lw.w.Flush(); err != nil {
		lw.abort(old.dir)
		return err
	}
	if err := lw.f.Sync(); err != nil {
		lw.abort(old.dir)
		return err
	}
	if err := os.Rename(filepath.Join(old.dir, logTmpName), filepath.Join(old.dir, logName)); err != nil {
		lw.abort(old.dir)
		return err
	}
	if err := syncDir(old.dir); err != nil {
		// The rename happened; the new file is the log either way.
		old.failed = err
	}
	_ = old.f.Close()
	old.f = lw.f
	old.size = lw.off
	if old.bg != nil {
		old.bg.swap(lw.f, lw.off)
	}
	return nil
}

func (lw *logWriter) abort(dir string) {
	lw.f.Close()
	_ = os.Remove(filepath.Join(dir, logTmpName))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

func fdatasync(f *os.File) error {
	for {
		err := syscall.Fdatasync(int(f.Fd()))
		if err != syscall.EINTR {
			return err
		}
	}
}
