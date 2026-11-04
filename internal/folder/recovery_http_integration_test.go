package folder_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/transport"
)

type httpState struct{ f *folder.Folder }

func (s httpState) Hello() transport.Hello {
	return transport.Hello{Protocol: 2, StoreEpoch: "test-store", PeerSetEpoch: "test-peers"}
}
func (s httpState) Index(_ context.Context, epoch string, since uint64, wait time.Duration, name string) (transport.IndexResponse, error) {
	s.f.Wait(since, wait)
	files, seq := s.f.Snapshot()
	return transport.IndexResponse{Protocol: 2, StoreEpoch: "test-store", Cursor: seq, Reset: epoch != "test-store", Files: files}, nil
}
func helloHTTP(t *testing.T, c *http.Client, addr string) {
	t.Helper()
	req, _ := http.NewRequest("POST", addr+"/hello", strings.NewReader(`{"protocol":2,"storeEpoch":"test","peerSetEpoch":"test","features":[]}`))
	req.Header.Set(transport.ProtocolHeader, "2")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatal("hello", resp.Status)
	}
}
func getHTTP(c *http.Client, addr string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", addr, nil)
	req.Header.Set(transport.ProtocolHeader, "2")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

func httpFolder(t *testing.T, size int64) (*folder.Folder, folder.FileInfo) {
	t.Helper()
	home := t.TempDir()
	f, err := folder.Open(filepath.Join(home, "root"), filepath.Join(home, "index.json"), "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	file, err := os.Create(filepath.Join(f.Root, "item"))
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(size)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	files, _ := f.Snapshot()
	return f, files[0]
}

func awaitHTTPBarrier(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP barrier timeout")
	}
}

func emptyHTTPFolder(t *testing.T, f *folder.Folder) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.Root, "item")); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err == nil || f.Blocked() == nil {
		t.Fatal("expected root_empty", err)
	}
}

func TestBlockedIndexAlreadyWaitingIntegration(t *testing.T) {
	f, _ := httpFolder(t, 1)
	entered := make(chan struct{})
	folder.SetWaitEntered(f, func() { close(entered) })
	_, seq := f.Snapshot()
	server := httptest.NewServer((&transport.Server{Folder: f, State: httpState{f}, PollTimeout: 10 * time.Second}).Handler())
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	helloHTTP(t, client, server.URL)
	done := make(chan struct{})
	var resp *http.Response
	var err error
	go func() {
		resp, err = getHTTP(client, server.URL+"/index?since="+strconv.FormatUint(seq, 10))
		close(done)
	}()
	awaitHTTPBarrier(t, entered)
	emptyHTTPFolder(t, f)
	awaitHTTPBarrier(t, done)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 503 || strings.Contains(string(body), `"files"`) {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
	}
}

// Flush the first real HTTP chunk, then hold the handler before its next read.
type firstChunkBarrier struct {
	http.ResponseWriter
	entered chan struct{}
	release <-chan struct{}
	first   bool
}

func (w *firstChunkBarrier) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *firstChunkBarrier) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if !w.first && n > 0 {
		w.first = true
		_ = http.NewResponseController(w.ResponseWriter).Flush()
		close(w.entered)
		<-w.release
	}
	return n, err
}

func TestRebindInterruptsHTTPFileIntegration(t *testing.T) {
	f, fi := httpFolder(t, 1<<20)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	h := (&transport.Server{Folder: f, State: httpState{f}}).Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file" {
			h.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(&firstChunkBarrier{ResponseWriter: w, entered: entered, release: release}, r)
	}))
	defer server.Close()
	defer unblock()
	client := &http.Client{Timeout: 5 * time.Second}
	helloHTTP(t, client, server.URL)
	resp, err := getHTTP(client, server.URL+"/file?name=item&hash="+fi.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	awaitHTTPBarrier(t, entered)
	first := make([]byte, 1024)
	if _, err = io.ReadFull(resp.Body, first); err != nil {
		t.Fatal("first HTTP chunk", err)
	}
	if resp.StatusCode != 200 || resp.ContentLength != fi.Size {
		t.Fatal("transfer not started", resp.Status)
	}
	generation := f.ConfirmedGeneration()
	emptyHTTPFolder(t, f)
	if err = f.Confirm(f.Blocked().Token, "rebind", f.Root); err != nil {
		t.Fatal(err)
	}
	if f.ConfirmedGeneration() == generation || f.Blocked() != nil {
		t.Fatal("rebind did not advance generation")
	}
	unblock()
	rest, err := io.ReadAll(resp.Body)
	if err == nil || os.IsTimeout(err) || int64(len(first)+len(rest)) >= fi.Size {
		t.Fatalf("old transfer completed: bytes=%d err=%v", len(first)+len(rest), err)
	}
}
