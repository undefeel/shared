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
	"strings"
	"testing"
	"time"

	"sharedir/internal/folder"
)

type batchWriter struct {
	*httptest.ResponseRecorder
	after   func()
	trigger []byte
}

func (w *batchWriter) SetWriteDeadline(_ time.Time) error { return nil }
func (w *batchWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	// Body is a separate write, after the header.
	if w.after != nil && bytes.Equal(p, w.trigger) {
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
func TestFilesStreaming(t *testing.T) {
	f, _ := fileFixture(t)
	t.Run("BoundedSequentialBodies", func(t *testing.T) { testFilesBoundedSequentialBodies(t, f) })
	t.Run("ChangesBetweenFrames", func(t *testing.T) { testFilesChangesBetweenFrames(t, f) })
}

func testFilesChangesBetweenFrames(t *testing.T, f *folder.Folder) {
	for _, mode := range []string{"version", "symlink", "blocked"} {
		for _, prefix := range []string{"a", "b"} {
			writeTestFile(t, filepath.Join(f.Root, prefix+mode), []byte("abc"), 0600)
		}
	}
	mustScan(t, f)
	for _, mode := range []string{"version", "symlink", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			fi, _, _ := f.Lookup("a" + mode)
			w := &batchWriter{ResponseRecorder: httptest.NewRecorder(), trigger: []byte("abc")}
			w.after = func() {
				switch mode {
				case "version":
					writeTestFile(t, filepath.Join(f.Root, "b"+mode), []byte("xyz"), 0600)
				case "blocked":
					requireNoError(t, os.Remove(filepath.Join(f.Root, "b"+mode)))
					entries, err := os.ReadDir(f.Root)
					requireNoError(t, err)
					for _, entry := range entries {
						if entry.Name() != "a" {
							requireNoError(t, os.Remove(filepath.Join(f.Root, entry.Name())))
						}
					}
					blockFileFixture(t, f)
				case "symlink":
					outside := filepath.Join(t.TempDir(), "secret")
					writeTestFile(t, outside, []byte("abc"), 0600)
					requireNoError(t, os.Remove(filepath.Join(f.Root, "b"+mode)))
					requireNoError(t, os.Symlink(outside, filepath.Join(f.Root, "b"+mode)))
				}
			}
			(&Server{Folder: f}).files(w, batchRequest([]BatchItem{{"1", "a" + mode, fi.Hash, 3}, {"2", "b" + mode, fi.Hash, 3}}))
			d := testBatchDecoder(t, bytes.NewReader(w.Body.Bytes()), map[string]struct{}{"1": {}, "2": {}})
			first, err := d.Next()
			if err != nil || string(first.Body) != "abc" || first.Header.ID != "1" {
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
			requireBatchEnd(t, d)
			if _, err := d.Next(); err == nil {
				t.Fatal("expected EOF after end")
			}
		})
	}
}

func TestSingleFileKnownTombstoneReturnsVersionChanged(t *testing.T) {
	f, fi := fileFixture(t)
	writeTestFile(t, filepath.Join(f.Root, "b"), []byte("keep"), 0600)
	requireNoError(t, os.Remove(filepath.Join(f.Root, "a")))
	mustScan(t, f)
	r := httptest.NewRequest("GET", "/file?name=a&hash="+fi.Hash, nil)
	w := httptest.NewRecorder()
	(&Server{Folder: f}).file(w, r)
	if w.Code != http.StatusConflict || strings.TrimSpace(w.Body.String()) != "version_changed" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
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
		requireEqualf(t, w.Code, 400, "status %d", w.Code)
	}
	blockFileFixture(t, f)
	w := &batchWriter{ResponseRecorder: httptest.NewRecorder()}
	(&Server{Folder: f}).files(w, batchRequest(nil))
	requireEqualf(t, w.Code, 503, "status %d", w.Code)
}
func testFilesBoundedSequentialBodies(t *testing.T, f *folder.Folder) {
	data := bytes.Repeat([]byte("z"), BatchMaxPlain)
	items := make([]BatchItem, 2)
	for i := range items {
		name := fmt.Sprint(i)
		writeTestFile(t, filepath.Join(f.Root, name), data, 0600)
		items[i] = BatchItem{ID: name, Name: name, Size: int64(len(data))}
	}
	mustScan(t, f)
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
	d := testBatchDecoder(t, pr, nil)
	for range items {
		frame, err := d.Next()
		if err != nil || !bytes.Equal(frame.Body, data) {
			t.Fatalf("frame %v", err)
		}
	}
	requireBatchEnd(t, d)
	<-done
}

type pipeBatchWriter struct{ io.Writer }

func (*pipeBatchWriter) Header() http.Header              { return make(http.Header) }
func (*pipeBatchWriter) WriteHeader(int)                  {}
func (*pipeBatchWriter) SetWriteDeadline(time.Time) error { return nil }
