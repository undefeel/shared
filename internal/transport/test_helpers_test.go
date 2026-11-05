package transport

import (
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

func testBatchDecoder(t testing.TB, r io.Reader, expected map[string]struct{}) *BatchDecoder {
	t.Helper()
	d, err := NewBatchDecoder(r, expected)
	requireNoError(t, err)
	return d
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

func requireBatchEnd(t testing.TB, d *BatchDecoder) {
	t.Helper()
	end, err := d.Next()
	if err != nil || !end.Header.End {
		t.Fatalf("end: %+v %v", end, err)
	}
}
