package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func clientFor(s *httptest.Server) (*Client, string) {
	return &Client{http: s.Client(), BodyIdleTimeout: 40 * time.Millisecond}, strings.TrimPrefix(s.URL, "https://")
}
func TestFetcherSizes(t *testing.T) {
	for _, size := range []int64{-1, 2, 3, 4} {
		t.Run(string(rune(size+65)), func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "abc") }))
			defer s.Close()
			c, addr := clientFor(s)
			err := c.Fetcher(addr)(context.Background(), folder.FileInfo{Size: size}, io.Discard)
			if (err == nil) != (size == 3) {
				t.Fatalf("size %d: %v", size, err)
			}
		})
	}
}
func TestIdleBody(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c, addr := clientFor(s)
	if err := c.Fetcher(addr)(context.Background(), folder.FileInfo{Size: 3}, io.Discard); err == nil {
		t.Fatal("expected idle timeout")
	}
}
func TestIndexLimits(t *testing.T) {
	for _, body := range []string{
		strings.Repeat(" ", 101),
		`{"seq":1,"files":[{"name":"` + strings.Repeat("a", 4097) + `"}]}`,
		`{"seq":1,"files":[{"version":{` + strings.Join(func() []string {
			var a []string
			for i := 0; i < 65; i++ {
				a = append(a, "\""+string(rune('A'+i))+"\":1")
			}
			return a
		}(), ",") + `}}]}`,
	} {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c, addr := clientFor(s)
		c.IndexLimit = 100
		if len(body) > 101 {
			c.IndexLimit = 10000
		}
		if _, err := c.Index(context.Background(), addr, 0); err == nil {
			t.Fatal("expected invalid index")
		}
		s.Close()
	}
}
func TestRequestRevocation(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	s := &Server{Allowed: func(string) bool { return allowed.Load() }, Paused: func() bool { return true }}
	cert := &x509.Certificate{Raw: []byte("peer")}
	h := s.Handler()
	request := func() int {
		r := httptest.NewRequest("GET", "https://peer/index", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !allowed.Load() && w.Header().Get("Connection") != "close" {
			t.Fatal("connection not closed")
		}
		return w.Code
	}
	if request() != 503 {
		t.Fatal("initial permission")
	}
	allowed.Store(false)
	if request() != 403 {
		t.Fatal("revoked permission")
	}
}
func TestHandshakeTimeoutAndLimit(t *testing.T) {
	g := &Connections{Max: 1, PerIP: 1}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.Config.ReadHeaderTimeout = 40 * time.Millisecond
	s.Listener = g.Limit(s.Listener)
	s.StartTLS()
	defer s.Close()
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Wait until the first socket has been admitted, without depending on scheduling.
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := g.count
		g.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accept timeout")
		}
		time.Sleep(time.Millisecond)
	}
	extra, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = extra.Read(make([]byte, 1)); err == nil {
		t.Fatal("limit not enforced")
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = c.Read(make([]byte, 1)); err == nil {
		t.Fatal("handshake not timed out")
	}
}
func TestRevokeTransfer(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	g := &Connections{}
	started := make(chan struct{})
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	s.Config.ConnState = g.State
	s.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	s.StartTLS()
	defer s.Close()
	cfg := s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.Certificates = s.TLS.Certificates
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer c.CloseIdleConnections()
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	allowed.Store(false)
	g.Revoke(func(string) bool { return allowed.Load() })
	if _, err = io.ReadAll(resp.Body); err == nil {
		t.Fatal("transfer survived revocation")
	}
}

// Keep-alive requests must recheck permission even without a new handshake.
func TestKeepAliveRevocation(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	handler := (&Server{Allowed: func(string) bool { return allowed.Load() }, Paused: func() bool { return true }}).Handler()
	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	s.StartTLS()
	defer s.Close()
	cfg := s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.Certificates = s.TLS.Certificates
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer c.CloseIdleConnections()
	var remote string
	original := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client runs these requests sequentially; report the socket in a header.
		w.Header().Set("X-Test-Remote", r.RemoteAddr)
		original.ServeHTTP(w, r)
	})
	for i, want := range []int{503, 403} {
		resp, err := c.Get(s.URL + "/index")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("got %d want %d", resp.StatusCode, want)
		}
		if i == 0 {
			remote = resp.Header.Get("X-Test-Remote")
			allowed.Store(false)
		} else if remote != resp.Header.Get("X-Test-Remote") {
			t.Fatal("connection was not reused")
		}
	}
}

func TestLongPollRevocation(t *testing.T) {
	f, err := folder.Open(t.TempDir(), filepath.Join(t.TempDir(), "state"), "self", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	_, seq := f.Snapshot()
	var calls atomic.Int32
	s := &Server{Folder: f, PollTimeout: time.Millisecond, Allowed: func(string) bool { return calls.Add(1) == 1 }}
	r := httptest.NewRequest("GET", fmt.Sprintf("https://peer/index?since=%d", seq), nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("peer")}}}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("long-poll returned %d", w.Code)
	}
}
