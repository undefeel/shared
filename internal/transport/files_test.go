package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type batchWriter struct {
	*httptest.ResponseRecorder
	after func()
}

func (w *batchWriter) SetWriteDeadline(_ time.Time) error { return nil }
func (w *batchWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	// Body is a separate write, after the header.
	if w.after != nil && string(p) == "abc" {
		f := w.after
		w.after = nil
		f()
	}
	return n, err
}
func batchRequest(items []BatchItem) *http.Request {
	b, _ := json.Marshal(struct {
		Protocol int         `json:"protocol"`
		Items    []BatchItem `json:"items"`
	}{2, items})
	r := httptest.NewRequest("POST", "/files", bytes.NewReader(b))
	r.Header.Set(ProtocolHeader, "2")
	return r
}
func TestFilesChangesBetweenFrames(t *testing.T) {
	for _, mode := range []string{"version", "blocked", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f, fi := fileFixture(t)
			if err := os.WriteFile(filepath.Join(f.Root, "b"), []byte("abc"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.Scan(); err != nil {
				t.Fatal(err)
			}
			w := &batchWriter{ResponseRecorder: httptest.NewRecorder()}
			w.after = func() {
				switch mode {
				case "version":
					if err := os.WriteFile(filepath.Join(f.Root, "b"), []byte("xyz"), 0600); err != nil {
						t.Fatal(err)
					}
				case "blocked":
					if err := os.Remove(filepath.Join(f.Root, "b")); err != nil {
						t.Fatal(err)
					}
					blockFileFixture(t, f)
				case "symlink":
					outside := filepath.Join(t.TempDir(), "secret")
					if err := os.WriteFile(outside, []byte("abc"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(f.Root, "b")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, filepath.Join(f.Root, "b")); err != nil {
						t.Fatal(err)
					}
				}
			}
			(&Server{Folder: f}).files(w, batchRequest([]BatchItem{{"1", "a", fi.Hash, 3}, {"2", "b", fi.Hash, 3}}))
			d, err := NewBatchDecoder(bytes.NewReader(w.Body.Bytes()), nil)
			if err != nil {
				t.Fatal(err)
			}
			first, err := d.Next()
			if err != nil || string(first.Body) != "abc" {
				t.Fatalf("first: %+v %v", first, err)
			}
			second, err := d.Next()
			want := "version_changed"
			if mode == "blocked" {
				want = "blocked"
			}
			if err != nil || second.Header.Status != want || len(second.Body) != 0 {
				t.Fatalf("second: %+v %v", second, err)
			}
			end, err := d.Next()
			if err != nil || !end.Header.End {
				t.Fatalf("end: %+v %v", end, err)
			}
		})
	}
}
func TestFilesLimitsAndBlocked(t *testing.T) {
	f, fi := fileFixture(t)
	for _, items := range [][]BatchItem{
		{{"1", "a", fi.Hash, BatchMaxPlain + 1}},
		{{"1", "a", fi.Hash, 3}, {"1", "b", fi.Hash, 3}},
	} {
		w := &batchWriter{ResponseRecorder: httptest.NewRecorder()}
		(&Server{Folder: f}).files(w, batchRequest(items))
		if w.Code != 400 {
			t.Fatalf("status %d", w.Code)
		}
	}
	blockFileFixture(t, f)
	w := &batchWriter{ResponseRecorder: httptest.NewRecorder()}
	(&Server{Folder: f}).files(w, batchRequest(nil))
	if w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
}
func TestFilesBoundedSequentialBodies(t *testing.T) {
	f, _ := fileFixture(t)
	data := bytes.Repeat([]byte("z"), BatchMaxPlain)
	items := make([]BatchItem, 32)
	for i := range items {
		name := fmt.Sprint(i)
		if err := os.WriteFile(filepath.Join(f.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		items[i] = BatchItem{ID: name, Name: name, Size: int64(len(data))}
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	files, _ := f.Snapshot()
	for _, fi := range files {
		for i := range items {
			if items[i].Name == fi.Name {
				items[i].Hash = fi.Hash
			}
		}
	}
	// A pipe forces each frame to be delivered before subsequent bodies can
	// accumulate; reader checks every frame's bounded payload and exact bytes.
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		(&Server{Folder: f}).files(&pipeBatchWriter{pw}, batchRequest(items))
	}()
	d, err := NewBatchDecoder(pr, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range items {
		frame, err := d.Next()
		if err != nil || !bytes.Equal(frame.Body, data) {
			t.Fatalf("frame %v", err)
		}
	}
	end, err := d.Next()
	if err != nil || !end.Header.End {
		t.Fatalf("end %v", err)
	}
	<-done
}

type pipeBatchWriter struct{ io.Writer }

func (*pipeBatchWriter) Header() http.Header              { return make(http.Header) }
func (*pipeBatchWriter) WriteHeader(int)                  {}
func (*pipeBatchWriter) SetWriteDeadline(time.Time) error { return nil }
