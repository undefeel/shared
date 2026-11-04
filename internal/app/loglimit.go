package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	logFileLimit = 5 << 20
	logFileCount = 3 // active file plus two rotated files
)

// BoundedLogWriter appends to path and keeps at most logFileCount files of
// logFileLimit bytes each. It is intended for diagnostic output, so it does
// not fsync individual writes.
type BoundedLogWriter struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	size        int64
	rename      func(string, string) error
	redirectFDs bool
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
	w := &BoundedLogWriter{path: path, file: f, size: st.Size(), rename: os.Rename}
	if w.size >= logFileLimit {
		if err := w.rotate(); err != nil {
			_ = w.Close()
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
	terminal := false
	redirect := false
	if f, ok := stderr.(*os.File); ok {
		_, ioctlErr := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		terminal = ioctlErr == nil
		redirect = !terminal
	}
	if terminal {
		return io.MultiWriter(w, stderr), w.Close, nil
	}
	if redirect {
		w.redirectFDs = true
		if err := w.redirectStandardFDs(); err != nil {
			_ = w.Close()
			return nil, nil, err
		}
	}
	return w, w.Close, nil
}

func (w *BoundedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureOpen(); err != nil {
		return 0, err
	}
	st, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	w.size = st.Size()
	if w.size > 0 && (w.size >= logFileLimit || int64(len(p)) > logFileLimit-w.size) {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := writeAll(w.file, p)
	w.size += int64(n)
	return n, err
}

func (w *BoundedLogWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			w.file = nil
			return w.reopenAfterRotateError(err)
		}
		w.file = nil
	}
	failed := func(err error) error { return w.reopenAfterRotateError(err) }
	if err := os.Remove(w.backup(logFileCount - 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed(err)
	}
	for i := logFileCount - 2; i >= 1; i-- {
		if err := w.rename(w.backup(i), w.backup(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failed(err)
		}
	}
	if err := w.rename(w.path, w.backup(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed(err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return failed(err)
	}
	w.file, w.size = f, 0
	if w.redirectFDs {
		if err := w.redirectStandardFDs(); err != nil {
			_ = f.Close()
			w.file = nil
			return failed(err)
		}
	}
	return nil
}

func (w *BoundedLogWriter) ensureOpen() error {
	if w.file != nil {
		return nil
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file, w.size = f, st.Size()
	if w.redirectFDs {
		if err := w.redirectStandardFDs(); err != nil {
			_ = f.Close()
			w.file = nil
			return err
		}
	}
	return nil
}

func (w *BoundedLogWriter) reopenAfterRotateError(cause error) error {
	if err := w.ensureOpen(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (w *BoundedLogWriter) redirectStandardFDs() error {
	if err := unix.Dup2(int(w.file.Fd()), 1); err != nil {
		return err
	}
	return unix.Dup2(int(w.file.Fd()), 2)
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
