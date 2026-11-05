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

func testLogWriter(t *testing.T, path string) *BoundedLogWriter {
	t.Helper()
	w, err := NewBoundedLogWriter(path)
	requireNoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func readTestLog(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	requireNoError(t, err)
	return b
}

func TestBoundedLogWriterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w := testLogWriter(t, path)

	for i := 0; i < logFileCount+1; i++ {
		mustWrite(t, w, bytes.Repeat([]byte{byte('a' + i)}, logFileLimit))
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
	got := readTestLog(t, path+".2")
	if got[0] != 'b' {
		t.Errorf("oldest retained log starts with %q, want b", got[0])
	}
}

func TestBoundedLogWriterRecoversAfterRotationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w := testLogWriter(t, path)
	original := bytes.Repeat([]byte("x"), logFileLimit)
	mustWrite(t, w, original)
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
	got := readTestLog(t, path)
	requireEqualf(t, string(got), "recovered", "active log = %q, want recovered", got)
	requireFileContents(t, path+".1", original, "original log after interrupted rotation")
}

func TestBoundedLogWriterPreservesOversizedExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	want := bytes.Repeat([]byte("legacy"), logFileLimit/6+17)
	writeTestFile(t, path, want, 0o600)
	testLogWriter(t, path)
	got := readTestLog(t, path+".1")
	if !bytes.Equal(got, want) {
		t.Fatalf("rotated legacy log has %d bytes, want %d", len(got), len(want))
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("new active log stat = %v, %v; want empty file", info, err)
	}
}

func TestBoundedLogWriterDoesNotSplitWriteAcrossFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w := testLogWriter(t, path)
	prefix := bytes.Repeat([]byte("p"), logFileLimit-2)
	mustWrite(t, w, prefix)
	mustWrite(t, w, []byte("whole-line"))
	active := readTestLog(t, path)
	rotated := readTestLog(t, path+".1")
	if string(active) != "whole-line" || !bytes.Equal(rotated, prefix) {
		t.Fatalf("active=%q rotated size=%d; line or prefix was split", active, len(rotated))
	}
}

func TestBoundedLogWriterAllowsSingleOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	w := testLogWriter(t, path)
	line := bytes.Repeat([]byte("L"), logFileLimit+1)
	if n, err := w.Write(line); err != nil || n != len(line) {
		t.Fatalf("oversized Write = (%d, %v), want %d bytes", n, err, len(line))
	}
	got := readTestLog(t, path)
	if !bytes.Equal(got, line) {
		t.Fatal("oversized line was split or changed")
	}
}

func TestBoundedLogWriterRedirectsStderrAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharedir.log")
	oldStdout, err := unix.Dup(1)
	requireNoError(t, err)
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
	requireNoError(t, err)
	w.redirectFDs = true
	requireNoError(t, w.redirectStandardFDs())
	mustWrite(t, w, bytes.Repeat([]byte("r"), logFileLimit))
	mustWrite(t, w, []byte("rotate"))
	mustWrite(t, os.Stderr, []byte("stderr-current"))
	requireNoError(t, w.Close())
	restore()
	active := readTestLog(t, path)
	rotated := readTestLog(t, path+".1")
	if !bytes.Contains(active, []byte("stderr-current")) || bytes.Contains(rotated, []byte("stderr-current")) {
		t.Fatal("stderr output did not land in the active log after rotation")
	}
}

func TestWriteAllHandlesPartialWrites(t *testing.T) {
	want := []byte("partial writes are completed")
	w := &shortWriter{limit: 3}
	n, err := writeAll(w, want)
	requireNoError(t, err)
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

func TestRunLogOutput(t *testing.T) {
	for _, tc := range []struct {
		name, platform, message, stderr string
		interactive, file               bool
	}{
		{"LinuxJournald", "linux", "journal only", "journal only", false, false},
		{"DarwinFile", "darwin", "file only", "", false, true},
		{"DarwinInteractive", "darwin", "interactive", "interactive", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sharedir.log")
			var stderr bytes.Buffer
			var writer io.Writer
			var closeOutput func() error
			var err error
			if tc.interactive {
				writer, closeOutput, err = newRunLogOutput(tc.platform, path, &stderr, true, false)
			} else {
				writer, closeOutput, err = NewRunLogOutput(tc.platform, path, &stderr)
			}
			requireNoError(t, err)
			mustWrite(t, writer, []byte(tc.message))
			requireNoError(t, closeOutput())
			requireEqualf(t, stderr.String(), tc.stderr, "stderr = %q, want %q", stderr.String(), tc.stderr)
			if tc.file {
				if got := readTestLog(t, path); string(got) != tc.message {
					t.Fatalf("file = %q, want %q", got, tc.message)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("log file exists or stat failed: %v", err)
			}
		})
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
