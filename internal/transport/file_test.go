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
	writeTestFile(t, filepath.Join(root, "a"), []byte("abc"), 0600)
	f, err := folder.Open(root, filepath.Join(t.TempDir(), "index"), "me", log.New(io.Discard, "", 0))
	requireNoError(t, err)
	t.Cleanup(func() { f.Close() })
	mustScan(t, f)
	files, _ := f.Snapshot()
	return f, files[0]
}
func blockFileFixture(t *testing.T, f *folder.Folder) {
	t.Helper()
	requireNoError(t, os.Remove(filepath.Join(f.Root, "a")))
	if err := f.Scan(); err == nil {
		t.Fatal("expected root_empty")
	}
}
func TestBlockedFileAndLongPoll(t *testing.T) {
	f, fi := fileFixture(t)
	t.Run("IndexAfterLongPoll", func(t *testing.T) { testBlockedIndexAfterLongPoll(t, f) })
	t.Run("File", func(t *testing.T) { testBlockedFile(t, f, fi) })
}

func testBlockedFile(t *testing.T, f *folder.Folder, fi folder.FileInfo) {
	w := httptest.NewRecorder()
	readyHandler(&Server{Folder: f, State: testState{f}}).ServeHTTP(w, v2Request("GET", "/file?name=a&hash="+fi.Hash, nil))
	requireEqualf(t, w.Code, 503, "status %d", w.Code)
}
func testBlockedIndexAfterLongPoll(t *testing.T, f *folder.Folder) {
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
	requireEqualf(t, w.Code, 503, "status %d", w.Code)
}
func TestV2SharedFixture(t *testing.T) {
	f, fi := fileFixture(t)
	t.Run("HelloRequired", func(t *testing.T) { testV2HelloRequired(t, f) })
	t.Run("FileAndIndexWireFormat", func(t *testing.T) { testV2FileAndIndexWireFormat(t, f, fi) })
	t.Run("NamedIndexRefresh", func(t *testing.T) { testNamedIndexRefresh(t, f, fi) })
	t.Run("AckAuthenticationAndValidation", func(t *testing.T) { testAckAuthenticationAndValidation(t, f) })
}

func testV2FileAndIndexWireFormat(t *testing.T, f *folder.Folder, fi folder.FileInfo) {
	server := httptest.NewServer(readyHandler(&Server{Folder: f, State: testState{f}}))
	defer server.Close()
	resp, err := v2Get(server.URL + "/file?name=a&hash=" + fi.Hash)
	requireNoError(t, err)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || string(data) != "abc" || resp.ContentLength != 3 {
		t.Fatalf("response %v %q %v", resp.Status, string(data), err)
	}
	resp, err = v2Get(server.URL + "/index")
	requireNoError(t, err)
	defer resp.Body.Close()
	var index IndexResponse
	requireNoError(t, json.NewDecoder(resp.Body).Decode(&index))
	if len(index.Files) != 1 || index.Files[0].Hash != fi.Hash || index.Files[0].Disk != "" {
		t.Fatalf("index %+v", index)
	}
}
