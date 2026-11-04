package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	logFileLimit = 5 << 20
	logFileCount = 3 // active file plus two rotated files
)

// BoundedLogWriter appends to path and keeps at most logFileCount files of
// logFileLimit bytes each. It is intended for diagnostic output, so it does
// not fsync individual writes.
type BoundedLogWriter struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func NewBoundedLogWriter(path string) (*BoundedLogWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &BoundedLogWriter{path: path, file: f, size: st.Size()}
	if w.size > logFileLimit {
		// An older unbounded log may predate this writer. Discard its oversized
		// contents rather than carrying an over-limit generation forward.
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		w.size = 0
	}
	if w.size == logFileLimit {
		if err := w.rotate(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return w, nil
}

// NewRunLogOutput keeps service output on stderr except on macOS, where the
// launch agent has no journal and diagnostics go to a bounded log file.
// The returned close function is always safe to call.
func NewRunLogOutput(goos, path string, stderr io.Writer) (io.Writer, func() error, error) {
	if goos != "darwin" {
		return stderr, func() error { return nil }, nil
	}
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		return nil, nil, err
	}
	return w, w.Close, nil
}

func (w *BoundedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	written := 0
	for len(p) > 0 {
		if w.size == logFileLimit {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}
		next := logFileLimit - w.size
		if int64(len(p)) < next {
			next = int64(len(p))
		}
		n, err := writeAll(w.file, p[:int(next)])
		w.size += int64(n)
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *BoundedLogWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		w.file = nil
		return err
	}
	w.file = nil
	if err := os.Remove(w.backup(logFileCount - 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := logFileCount - 2; i >= 1; i-- {
		if err := os.Rename(w.backup(i), w.backup(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(w.path, w.backup(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *BoundedLogWriter) backup(n int) string { return fmt.Sprintf("%s.%d", w.path, n) }

func (w *BoundedLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func writeAll(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
