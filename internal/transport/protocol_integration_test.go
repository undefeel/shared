package transport

import (
	"bytes"
	"context"
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

type testState struct{ f *folder.Folder }

func (s testState) Hello() Hello {
	return Hello{Protocol: 2, StoreEpoch: "store", PeerSetEpoch: "peers", Features: []string{"files", "gzip"}}
}
func (s testState) Index(ctx context.Context, epoch string, since uint64, wait time.Duration, name string) (IndexResponse, error) {
	if name == "" {
		s.f.Wait(since, wait)
	}
	files, seq := s.f.Snapshot()
	if name != "" {
		files = nil
		if fi, _, ok := s.f.Lookup(name); ok {
			files = append(files, fi)
		}
	}
	return IndexResponse{Protocol: 2, StoreEpoch: "store", Cursor: seq, Reset: epoch != "store", Files: files}, nil
}
func v2Request(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set(ProtocolHeader, "2")
	return r
}
func readyHandler(s *Server) http.Handler {
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.sessions.Store(connectionKey(r), struct{}{})
		h.ServeHTTP(w, r)
	})
}
func v2Get(target string) (*http.Response, error) {
	r, _ := http.NewRequest("GET", target, nil)
	r.Header.Set(ProtocolHeader, "2")
	return http.DefaultClient.Do(r)
}
func TestV2HelloRequired(t *testing.T) {
	f, _ := fileFixture(t)
	s := &Server{Folder: f, State: testState{f}}
	h := s.Handler()
	for _, tc := range []struct {
		version string
		want    int
	}{{"", 426}, {"1", 426}, {"2", 428}} {
		r := httptest.NewRequest("GET", "/index", nil)
		r.Header.Set(ProtocolHeader, tc.version)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("version %q: %d", tc.version, w.Code)
		}
	}
	r := v2Request("POST", "/hello", strings.NewReader(`{"protocol":2,"storeEpoch":"client","peerSetEpoch":"peers","features":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	r = v2Request("GET", "/index?name=a&since=999", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cursor":`) || strings.Contains(w.Body.String(), `"seq":`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestOldServerHello404(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	c := srv.Client()
	c.Transport = &helloTransport{base: c.Transport}
	_, err := c.Get(srv.URL + "/index")
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestV2GzipSingleFile(t *testing.T) {
	f, _ := fileFixture(t)
	plain := strings.Repeat("compressible text\n", 4096)
	if err := os.WriteFile(filepath.Join(f.Root, "a"), []byte(plain), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	files, _ := f.Snapshot()
	srv := httptest.NewTLSServer((&Server{Folder: f, State: testState{f}}).Handler())
	defer srv.Close()
	hc := srv.Client()
	base := hc.Transport
	var encoding string
	hc.Transport = &helloTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Accept-Encoding", "gzip")
		resp, err := base.RoundTrip(r)
		if err == nil && r.URL.Path == "/file" {
			encoding = resp.Header.Get("Content-Encoding")
		}
		return resp, err
	})}
	c := &Client{http: hc}
	var out bytes.Buffer
	if err := c.Fetcher(strings.TrimPrefix(srv.URL, "https://"))(context.Background(), files[0], &out); err != nil {
		t.Fatal(err)
	}
	if encoding != "gzip" || out.String() != plain {
		t.Fatalf("encoding %q decoded length %d", encoding, out.Len())
	}
}
func TestNamedIndexRefresh(t *testing.T) {
	f, fi := fileFixture(t)
	srv := httptest.NewTLSServer((&Server{Folder: f, State: testState{f}}).Handler())
	defer srv.Close()
	hc := srv.Client()
	hc.Transport = &helloTransport{base: hc.Transport}
	c := &Client{http: hc}
	out, err := c.IndexSince(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "store", 999, "a")
	if err != nil {
		t.Fatal(err)
	}
	if out.Reset || out.StoreEpoch != "store" || len(out.Files) != 1 || out.Files[0].Hash != fi.Hash || out.Cursor == 999 {
		t.Fatalf("%+v", out)
	}
}
