package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clientForFiles(t *testing.T, h http.HandlerFunc) (*Client, string, func()) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	c := &Client{http: s.Client(), port: 1234}
	return c, strings.TrimPrefix(s.URL, "https://"), s.Close
}

func batchItem(id, name, body string) BatchItem {
	h := sha256.Sum256([]byte(body))
	return BatchItem{ID: id, Name: name, Hash: hex.EncodeToString(h[:]), Size: int64(len(body))}
}

func readBatchIDs(r io.Reader) ([]BatchItem, error) {
	var req struct {
		Protocol int         `json:"protocol"`
		Items    []BatchItem `json:"items"`
	}
	err := json.NewDecoder(r).Decode(&req)
	return req.Items, err
}

func writeFileFrame(w http.ResponseWriter, item BatchItem, body string) {
	_ = WriteBatchFrame(w, BatchHeader{ID: item.ID, Status: "ok", Name: item.Name, Hash: item.Hash, Size: item.Size, Encoding: "identity", WireSize: int64(len(body))}, []byte(body))
}

func TestFetchFilesKeepsGoodFramesAndRetriesOnlyRemaining(t *testing.T) {
	a, b := batchItem("a", "a.txt", "alpha"), batchItem("b", "b.txt", "bravo")
	var calls atomic.Int32
	c, addr, closeServer := clientForFiles(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-sharedir-files;version=2")
		items, err := readBatchIDs(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := WriteBatchMagic(w); err != nil {
			return
		}
		if calls.Add(1) == 1 {
			if len(items) != 2 {
				t.Errorf("first request has %d items", len(items))
			}
			writeFileFrame(w, a, "alpha")
			w.(http.Flusher).Flush()
			<-r.Context().Done() // the client's idle watchdog cancels this attempt
			return
		}
		if len(items) != 1 || items[0].ID != "b" {
			t.Errorf("retry items = %+v", items)
		}
		writeFileFrame(w, b, "bravo")
		_ = WriteBatchFrame(w, BatchHeader{End: true, Count: 1}, nil)
	})
	defer closeServer()
	c.BodyIdleTimeout = 20 * time.Millisecond
	got, err := c.FetchFiles(context.Background(), addr, []BatchItem{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if string(got["a"].Body) != "alpha" || string(got["b"].Body) != "bravo" || calls.Load() != 2 {
		t.Fatalf("results=%+v calls=%d", got, calls.Load())
	}
}

func TestFetchFilesCancellationStopsStalledRequest(t *testing.T) {
	a, b := batchItem("a", "a.txt", "alpha"), batchItem("b", "b.txt", "bravo")
	started := make(chan struct{})
	stopped := make(chan struct{})
	c, addr, closeServer := clientForFiles(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-sharedir-files;version=2")
		_, _ = readBatchIDs(r.Body)
		_ = WriteBatchMagic(w)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	defer closeServer()
	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		got map[string]BatchResult
		err error
	}
	done := make(chan response, 1)
	go func() { got, err := c.FetchFiles(ctx, addr, []BatchItem{a, b}); done <- response{got, err} }()
	<-started
	cancel()
	result := <-done
	<-stopped
	if result.err != context.Canceled || len(result.got) != 0 {
		t.Fatalf("results=%+v err=%v", result.got, result.err)
	}
}
