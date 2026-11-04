package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
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
