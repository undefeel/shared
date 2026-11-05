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
	"runtime"
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
			requireEqualf(t, (err == nil), (size == 3), "size %d: %v", size, err)
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
func indexClient(body string, limit int64) (*Client, string, func()) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	c, addr := clientFor(s)
	c.IndexLimit = limit
	return c, addr, s.Close
}

func TestIndexStreamingRecords(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"seq":77,"files":[`)
	for i := 0; i < 10000; i++ {
		if i != 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"name":"file-%d","size":%d,"version":{"peer":%d}}`, i, i, i)
	}
	body.WriteString(`]}`)
	c, addr, closeServer := indexClient(body.String(), int64(body.Len()+1))
	defer closeServer()
	out, err := c.Index(context.Background(), addr, 0)
	requireNoError(t, err)
	if out.Seq != 77 || len(out.Files) != 10000 || out.Files[9999].Name != "file-9999" || out.Files[9999].Size != 9999 || out.Files[9999].Version["peer"] != 9999 {
		t.Fatalf("unexpected decoded index: seq=%d files=%d last=%+v", out.Seq, len(out.Files), out.Files[len(out.Files)-1])
	}
}

func TestIndexStreamingRejectsLimitAndMalformedRecords(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int64
		want       string
	}{
		{"body limit", `{"seq":1,"files":[]}` + strings.Repeat(" ", 100), 20, "index exceeds size limit"},
		{"long name", `{"seq":1,"files":[{"name":"ok"},{"name":"` + strings.Repeat("x", 4097) + `"},{"name":"unread"}]}`, 10000, "invalid index record"},
		{"bad JSON", `{"seq":1,"files":[`, 100, ""},
		{"too many vector entries", `{"seq":1,"files":[{"version":{` + strings.Join(func() []string {
			var keys []string
			for i := 0; i < 65; i++ {
				keys = append(keys, fmt.Sprintf("\"%c\":1", 'A'+i))
			}
			return keys
		}(), ",") + `}}]}`, 10000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, addr, closeServer := indexClient(tc.body, tc.limit)
			defer closeServer()
			_, err := c.Index(context.Background(), addr, 0)
			if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestIndexDoesNotCopyRawBody(t *testing.T) {
	const nameSize, count = 4096, 5120
	name := strings.Repeat("n", nameSize)
	var body strings.Builder
	body.Grow(count * (nameSize + 30))
	body.WriteString(`{"seq":1,"files":[`)
	for i := 0; i < count; i++ {
		if i != 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"name":"`)
		body.WriteString(name)
		body.WriteString(`","version":{}}`)
	}
	body.WriteString(`]}`)
	if body.Len() < 20<<20 {
		t.Fatalf("test body is only %d bytes", body.Len())
	}
	c, addr, closeServer := indexClient(body.String(), int64(body.Len()+1))
	defer closeServer()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := c.Index(context.Background(), addr, 0)
	runtime.ReadMemStats(&after)
	requireNoError(t, err)
	requireEqualf(t, len(out.Files), count, "got %d records", len(out.Files))
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > uint64(body.Len())*17/10 {
		t.Fatalf("allocated %d bytes for %d-byte index; likely copied the raw body", allocated, body.Len())
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
	requireNoError(t, err)
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
	requireNoError(t, err)
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

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

type testAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c testAddrConn) RemoteAddr() net.Addr { return c.remote }

type testListener struct{ conns []net.Conn }

func (l *testListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, io.EOF
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}
func (*testListener) Close() error   { return nil }
func (*testListener) Addr() net.Addr { return testAddr("listener") }

func queuedConns(n int, addr net.Addr) (*testListener, []net.Conn) {
	l := &testListener{}
	peers := make([]net.Conn, 0, n)
	for range n {
		c, peer := net.Pipe()
		l.conns = append(l.conns, testAddrConn{Conn: c, remote: addr})
		peers = append(peers, peer)
	}
	return l, peers
}

func closeConns(cs []net.Conn) {
	for _, c := range cs {
		c.Close()
	}
}

func TestRelayConnectionLimits(t *testing.T) {
	g := &Connections{}
	addr := testAddr("192.0.2.1:1234")
	l, peers := queuedConns(21, addr)
	defer closeConns(peers)
	ln := g.LimitRelay(l, 20)
	accepted := make([]net.Conn, 0, 20)
	for range 20 {
		c, err := ln.Accept()
		if err != nil {
			t.Fatalf("relay accept: %v", err)
		}
		accepted = append(accepted, c)
	}
	if _, err := ln.Accept(); err == nil {
		t.Fatal("relay limit not enforced")
	}
	accepted[0].Close()
	replacement, peer := net.Pipe()
	defer peer.Close()
	l.conns = append(l.conns, testAddrConn{Conn: replacement, remote: addr})
	reused, err := ln.Accept()
	if err != nil {
		t.Fatalf("closed relay slot was not reused: %v", err)
	}
	reused.Close()
	for _, c := range accepted {
		c.Close()
	}
	if g.count != 0 || g.relayCount != 0 {
		t.Fatalf("relay slots not released: total=%d relay=%d", g.count, g.relayCount)
	}
}

func TestDirectPerIPAndSharedConnectionLimit(t *testing.T) {
	g := &Connections{Max: 18}
	addr := testAddr("192.0.2.2:1234")
	direct, directPeers := queuedConns(17, addr)
	defer closeConns(directPeers)
	directLn := g.Limit(direct)
	acceptedDirect := make([]net.Conn, 0, 16)
	for range 16 {
		c, err := directLn.Accept()
		if err != nil {
			t.Fatalf("direct accept: %v", err)
		}
		acceptedDirect = append(acceptedDirect, c)
	}
	if _, err := directLn.Accept(); err == nil {
		t.Fatal("direct per-IP limit not enforced")
	}
	// Existing direct connections leave two global slots for relay channels;
	// the next relay channel is rejected even though its own cap is free.
	relay, relayPeers := queuedConns(3, addr)
	defer closeConns(relayPeers)
	relayLn := g.LimitRelay(relay, 64)
	acceptedRelay := make([]net.Conn, 0, 2)
	for range 2 {
		c, err := relayLn.Accept()
		if err != nil {
			t.Fatalf("relay accept under shared limit: %v", err)
		}
		acceptedRelay = append(acceptedRelay, c)
	}
	if _, err := relayLn.Accept(); err == nil {
		t.Fatal("shared total limit not enforced")
	}
	requireEqualf(t, g.count, 18, "total count=%d, want 18", g.count)
	for _, c := range acceptedDirect {
		c.Close()
	}
	for _, c := range acceptedRelay {
		c.Close()
	}
}

func revocationClient(t *testing.T, s *httptest.Server) *http.Client {
	t.Helper()
	s.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	s.StartTLS()
	t.Cleanup(s.Close)
	cfg := s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.Certificates = s.TLS.Certificates
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	t.Cleanup(c.CloseIdleConnections)
	return c
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
	c := revocationClient(t, s)
	resp, err := c.Get(s.URL)
	requireNoError(t, err)
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
	server := &Server{Allowed: func(string) bool { return allowed.Load() }, Paused: func() bool { return true }}
	handler := server.Handler()
	s := httptest.NewUnstartedServer(handler)
	var remote string
	original := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client runs these requests sequentially; report the socket in a header.
		w.Header().Set("X-Test-Remote", r.RemoteAddr)
		original.ServeHTTP(w, r)
	})
	c := revocationClient(t, s)
	for i, want := range []int{503, 403} {
		resp, err := c.Get(s.URL + "/index")
		requireNoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		requireEqualf(t, resp.StatusCode, want, "got %d want %d", resp.StatusCode, want)
		if i == 0 {
			remote = resp.Header.Get("X-Test-Remote")
			allowed.Store(false)
		} else if remote != resp.Header.Get("X-Test-Remote") {
			t.Fatal("connection was not reused")
		}
	}
	// The direct handler also instructs a rejected peer to close the connection.
	r := v2Request("GET", "https://peer/index", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("peer")}}}
	w := httptest.NewRecorder()
	readyHandler(server).ServeHTTP(w, r)
	if w.Code != 403 || w.Header().Get("Connection") != "close" {
		t.Fatalf("revoked: status=%d connection=%q", w.Code, w.Header().Get("Connection"))
	}

}

func TestLongPollRevocation(t *testing.T) {
	f, err := folder.Open(t.TempDir(), filepath.Join(t.TempDir(), "state"), "self", log.New(io.Discard, "", 0))
	requireNoError(t, err)
	_, seq := f.Snapshot()
	var calls atomic.Int32
	s := &Server{Folder: f, State: testState{f}, PollTimeout: time.Millisecond, Allowed: func(string) bool { return calls.Add(1) == 1 }}
	r := v2Request("GET", fmt.Sprintf("https://peer/index?since=%d", seq), nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("peer")}}}
	w := httptest.NewRecorder()
	readyHandler(s).ServeHTTP(w, r)
	requireEqualf(t, w.Code, 403, "long-poll returned %d", w.Code)
}
