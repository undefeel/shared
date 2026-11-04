package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBoundedLogWriterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < logFileCount+1; i++ {
		if _, err := w.Write(bytes.Repeat([]byte{byte('a' + i)}, logFileLimit)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < logFileCount; i++ {
		name := path
		if i > 0 {
			name += "." + string(rune('0'+i))
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() != logFileLimit {
			t.Errorf("%s size = %d, want %d", name, info.Size(), logFileLimit)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("unexpected fourth generation: err=%v", err)
	}
	got, err := os.ReadFile(path + ".2")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 'b' {
		t.Errorf("oldest retained log starts with %q, want b", got[0])
	}
}

func TestBoundedLogWriterRecoversAfterRotationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write(bytes.Repeat([]byte("x"), logFileLimit)); err != nil {
		t.Fatal(err)
	}
	originalRename := w.rename
	failed := false
	w.rename = func(old, new string) error {
		if old == path && !failed {
			failed = true
			return os.ErrPermission
		}
		return originalRename(old, new)
	}
	if _, err := w.Write([]byte("retry")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("first write error = %v, want injected rotation error", err)
	}
	if _, err := w.Write([]byte("recovered")); err != nil {
		t.Fatalf("write after failed rotation: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "recovered" {
		t.Fatalf("active log = %q, want recovered", got)
	}
}

func TestBoundedLogWriterPreservesOversizedExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	want := bytes.Repeat([]byte("legacy"), logFileLimit/6+17)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rotated legacy log has %d bytes, want %d", len(got), len(want))
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("new active log stat = %v, %v; want empty file", info, err)
	}
}

func TestBoundedLogWriterDoesNotSplitWriteAcrossFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	prefix := bytes.Repeat([]byte("p"), logFileLimit-2)
	if _, err := w.Write(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("whole-line")); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != "whole-line" || !bytes.Equal(rotated, prefix) {
		t.Fatalf("active=%q rotated size=%d; line or prefix was split", active, len(rotated))
	}
}

func TestBoundedLogWriterAllowsSingleOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := bytes.Repeat([]byte("L"), logFileLimit+1)
	if n, err := w.Write(line); err != nil || n != len(line) {
		t.Fatalf("oversized Write = (%d, %v), want %d bytes", n, err, len(line))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, line) {
		t.Fatal("oversized line was split or changed")
	}
}

func TestBoundedLogWriterRedirectsStderrAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	oldStdout, err := unix.Dup(1)
	if err != nil {
		t.Fatal(err)
	}
	oldStderr, err := unix.Dup(2)
	if err != nil {
		_ = unix.Close(oldStdout)
		t.Fatal(err)
	}
	restore := func() {
		if oldStdout < 0 {
			return
		}
		_ = unix.Dup2(oldStdout, 1)
		_ = unix.Dup2(oldStderr, 2)
		_ = unix.Close(oldStdout)
		_ = unix.Close(oldStderr)
		oldStdout, oldStderr = -1, -1
	}
	t.Cleanup(restore)
	w, err := NewBoundedLogWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w.redirectFDs = true
	if err := w.redirectStandardFDs(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("r"), logFileLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("rotate")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stderr.Write([]byte("stderr-current")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	restore()
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(active, []byte("stderr-current")) || bytes.Contains(rotated, []byte("stderr-current")) {
		t.Fatal("stderr output did not land in the active log after rotation")
	}
}

func TestWriteAllHandlesPartialWrites(t *testing.T) {
	want := []byte("partial writes are completed")
	w := &shortWriter{limit: 3}
	n, err := writeAll(w, want)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || !bytes.Equal(w.buf.Bytes(), want) {
		t.Fatalf("got %d bytes %q, want %q", n, w.buf.Bytes(), want)
	}
}

func TestWriteAllRejectsZeroProgress(t *testing.T) {
	n, err := writeAll(zeroWriter{}, []byte("x"))
	if n != 0 || err != io.ErrShortWrite {
		t.Fatalf("got (%d, %v), want (0, ErrShortWrite)", n, err)
	}
}

func TestRunLogOutputKeepsLinuxOnJournaldStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	var stderr bytes.Buffer
	writer, closeOutput, err := NewRunLogOutput("linux", path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("journal only")); err != nil {
		t.Fatal(err)
	}
	if err := closeOutput(); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "journal only" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("log file exists or stat failed: %v", err)
	}
}

func TestRunLogOutputUsesBoundedFileOnDarwin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	var stderr bytes.Buffer
	writer, closeOutput, err := NewRunLogOutput("darwin", path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("file only")); err != nil {
		t.Fatal(err)
	}
	if err := closeOutput(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "file only" || stderr.Len() != 0 {
		t.Fatalf("file = %q, stderr = %q", got, stderr.String())
	}
}

func TestRunLogOutputIncludesStderrWhenInteractive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	var stderr bytes.Buffer
	writer, closeOutput, err := newRunLogOutput("darwin", path, &stderr, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("interactive")); err != nil {
		t.Fatal(err)
	}
	if err := closeOutput(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "interactive" || stderr.String() != "interactive" {
		t.Fatalf("file=%q stderr=%q", got, stderr.String())
	}
}

type shortWriter struct {
	limit int
	buf   bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	return w.buf.Write(p)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
