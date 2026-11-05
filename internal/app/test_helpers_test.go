package app

import (
	"bytes"
	"io"
	"os"
	"sharedir/internal/folder"
	"testing"
)

func requireNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func writeTestFile(t testing.TB, path string, body []byte, mode os.FileMode) {
	t.Helper()
	requireNoError(t, os.WriteFile(path, body, mode))
}
func mustMkdirAll(t testing.TB, path string, mode os.FileMode) {
	t.Helper()
	requireNoError(t, os.MkdirAll(path, mode))
}
func mustScan(t testing.TB, f *folder.Folder) {
	t.Helper()
	requireNoError(t, f.Scan())
}

func mustWrite(t testing.TB, w io.Writer, b []byte) {
	t.Helper()
	_, err := w.Write(b)
	requireNoError(t, err)
}

func requireEqual[T comparable](t testing.TB, got, want T, detail ...any) {
	t.Helper()
	if got != want {
		t.Fatal(detail...)
	}
}
func requireEqualf[T comparable](t testing.TB, got, want T, format string, args ...any) {
	t.Helper()
	if got != want {
		t.Fatalf(format, args...)
	}
}

func requireMissingFile(t testing.TB, path, label string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: path=%q err=%v", label, path, err)
	}
}

func requireFileContents(t testing.TB, path string, want []byte, label string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: path=%q err=%v bytes=%d got=%q", label, path, err, len(got), got)
	}
}
