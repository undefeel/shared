package transport

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func fileFixture(t *testing.T) (*folder.Folder, folder.FileInfo) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := folder.Open(root, filepath.Join(t.TempDir(), "index"), "me", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	files, _ := f.Snapshot()
	return f, files[0]
}
func blockFileFixture(t *testing.T, f *folder.Folder) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.Root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err == nil {
		t.Fatal("expected root_empty")
	}
}
func TestBlockedFile(t *testing.T) {
	f, fi := fileFixture(t)
	blockFileFixture(t, f)
	w := httptest.NewRecorder()
	readyHandler(&Server{Folder: f, State: testState{f}}).ServeHTTP(w, v2Request("GET", "/file?name=a&hash="+fi.Hash, nil))
	if w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
}
func TestBlockedIndexAfterLongPoll(t *testing.T) {
	f, _ := fileFixture(t)
	_, seq := f.Snapshot()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	s := &Server{Folder: f, State: testState{f}, PollTimeout: time.Second}
	go func() {
		readyHandler(s).ServeHTTP(w, v2Request("GET", "/index?since="+strconv.FormatUint(seq, 10), nil))
		close(done)
	}()
	blockFileFixture(t, f)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked poll not woken")
	}
	if w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
}
func TestV2FileAndIndexWireFormat(t *testing.T) {
	f, fi := fileFixture(t)
	server := httptest.NewServer(readyHandler(&Server{Folder: f, State: testState{f}}))
	defer server.Close()
	resp, err := v2Get(server.URL + "/file?name=a&hash=" + fi.Hash)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || string(data) != "abc" || resp.ContentLength != 3 {
		t.Fatalf("response %v %q %v", resp.Status, string(data), err)
	}
	resp, err = v2Get(server.URL + "/index")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var index IndexResponse
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatal(err)
	}
	if len(index.Files) != 1 || index.Files[0].Hash != fi.Hash || index.Files[0].Disk != "" {
		t.Fatalf("index %+v", index)
	}
}
