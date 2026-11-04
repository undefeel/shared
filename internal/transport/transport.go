// Package transport — обмен индексами и файлами между устройствами поверх
// HTTPS со взаимной аутентификацией. Сертификаты самоподписанные; доверие
// строится на ID устройства (хеше сертификата), а не на CA.
//
// Модель «тянем»: каждое устройство держит long-poll на /index пира и само
// скачивает недостающее через /file.
package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"

	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/identity"
)

const (
	// MaxIndexBytes allows one million ordinary file records (512 bytes each).
	MaxIndexBytes   int64 = 512 << 20
	BodyIdleTimeout       = 30 * time.Second
	pollTimeout           = 50 * time.Second
	// portHeader сообщает серверу, на каком порту слушает клиент: так
	// сторона, к которой подключились, узнаёт обратный адрес пира и адрес
	// достаточно прописать вручную только на одной машине.
	portHeader = "X-Sharedir-Port"
)

type IndexResponse struct {
	Seq        uint64            `json:"-"` // compatibility alias for callers; cursor is the wire field
	Protocol   int               `json:"protocol"`
	StoreEpoch string            `json:"storeEpoch"`
	Cursor     uint64            `json:"cursor"`
	Reset      bool              `json:"reset"`
	Files      []folder.FileInfo `json:"files"`
}

// StateProvider must return durable epochs (stable across restart) and an atomic
// index snapshot/cursor pair. Index implements epoch mismatch/compacted cursor
// reset with a full baseline, bounded wait, cancellation, and exact-name refresh.
// Returned records must omit local Disk/binding metadata. Calls run without
// transport locks; the daemon supplies the real store adapter.
type StateProvider interface {
	Hello() Hello
	Index(context.Context, string, uint64, time.Duration, string) (IndexResponse, error)
}

func peerID(rawCerts [][]byte) (string, error) {
	if len(rawCerts) == 0 {
		return "", errors.New("пир не предъявил сертификат")
	}
	return identity.IDFromCert(rawCerts[0]), nil
}

// ServerTLS принимает только клиентов, для которых allowed(id) == true.
// Остальные отклоняются на рукопожатии, а onReject узнаёт их ID —
// так незнакомое устройство можно предложить к сопряжению.
func ServerTLS(id *identity.Identity, allowed func(string) bool, onReject func(id string)) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pid, err := peerID(raw)
			if err != nil {
				return err
			}
			if !allowed(pid) {
				onReject(pid)
				return fmt.Errorf("устройство %s не сопряжено", identity.ShortID(pid))
			}
			return nil
		},
	}
}

// RelayHost — псевдо-адрес пира «через relay». Клиент, получив его,
// дозванивается не по TCP, а через relayDial.
const RelayHost = "relay"

// ErrPeerPaused — пир поставил синхронизацию на паузу.
var ErrPeerPaused = errors.New("пир на паузе")

type Server struct {
	State            StateProvider
	sessions         sync.Map // bounded, keyed by TLS connection state (never JSON identity)
	Allowed          func(string) bool
	WriteIdleTimeout time.Duration
	PollTimeout      time.Duration
	Folder           *folder.Folder
	Log              *log.Logger
	// OnPeer вызывается на каждый запрос с ID пира и его обратным адресом.
	OnPeer func(id, addr string)
	// Paused — пока true, пиру отвечаем 503 и ничего не отдаём.
	Paused func() bool
	// OnServed вызывается, когда пир полностью скачал файл.
	OnServed func(peerID string, fi folder.FileInfo)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hello", s.hello)
	mux.HandleFunc("POST /files", s.files)
	mux.HandleFunc("GET /index", s.index)
	mux.HandleFunc("GET /file", s.file)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorize(w, r) {
			return
		}
		if (s.Paused != nil && s.Paused()) || (s.Folder != nil && s.Folder.Blocked() != nil) {
			http.Error(w, "paused", http.StatusServiceUnavailable)
			return
		}
		if s.OnPeer != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if port, err := strconv.Atoi(r.Header.Get(portHeader)); err == nil && port > 0 && host != "" {
				s.OnPeer(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), net.JoinHostPort(host, strconv.Itoa(port)))
			}
		}
		if r.Header.Get(ProtocolHeader) != "2" {
			WriteProtocolIncompatible(w, r.Header.Get(ProtocolHeader))
			return
		}
		if r.URL.Path != ProtocolHelloPath {
			if _, ok := s.sessions.Load(connectionKey(r)); !ok {
				http.Error(w, "hello required", http.StatusPreconditionRequired)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.Allowed == nil {
		return true
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && s.Allowed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw)) {
		return true
	}
	w.Header().Set("Connection", "close")
	http.Error(w, "access revoked", http.StatusForbidden)
	return false
}

func connectionKey(r *http.Request) any {
	if r.TLS != nil {
		return r.TLS
	}
	return r.RemoteAddr // HTTP-only test fixtures
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	if s.State == nil {
		http.Error(w, "state unavailable", 503)
		return
	}
	if _, err := DecodeHello(r.Body); err != nil {
		var pe *ProtocolError
		if errors.As(err, &pe) {
			WriteProtocolIncompatible(w, pe.Received)
		} else {
			http.Error(w, err.Error(), 400)
		}
		return
	}
	// Keep negotiation bookkeeping bounded even if the listener does not expose
	// connection-close callbacks. Eviction fails closed and requires a fresh hello.
	count := 0
	s.sessions.Range(func(k, v any) bool {
		count++
		if count >= 128 {
			s.sessions.Delete(k)
		}
		return true
	})
	s.sessions.Store(connectionKey(r), struct{}{})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.State.Hello())
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if s.State == nil {
		http.Error(w, "state unavailable", 503)
		return
	}
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	timeout := s.PollTimeout
	if timeout <= 0 {
		timeout = pollTimeout
	}
	if wait, err := time.ParseDuration(q.Get("wait") + "s"); err == nil && wait >= 0 && wait < timeout {
		timeout = wait
	}
	generation := s.Folder.ConfirmedGeneration()
	out, err := s.State.Index(r.Context(), q.Get("epoch"), since, timeout, q.Get("name"))
	if !s.authorize(w, r) {
		return
	}
	if err != nil || (s.Paused != nil && s.Paused()) || s.Folder.CheckRead(generation) != nil {
		http.Error(w, "folder unavailable", 503)
		return
	}
	out.Protocol = ProtocolVersion
	for i := range out.Files {
		out.Files[i].Disk = ""
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	generation := s.Folder.ConfirmedGeneration()
	fi, f, err := s.Folder.OpenVersion(q.Get("name"), q.Get("hash"))
	if err != nil || s.Folder.CheckRead(generation) != nil {
		if f != nil {
			f.Close()
		}
		status := http.StatusConflict
		if s.Folder.Blocked() != nil || s.Folder.CheckRead(generation) != nil {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "file unavailable", status)
		return
	}
	defer f.Close()
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/octet-stream")
	var dst io.Writer = &versionWriter{progressWriter: progressWriter{w: w, idle: s.writeIdle()}, folder: s.Folder, generation: generation}
	input := bufio.NewReaderSize(f, compressionSampleSize)
	compress := false
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && fi.Size >= 1024 && !knownIncompressible(fi.Name) {
		sample, err := input.Peek(int(min(fi.Size, compressionSampleSize)))
		if err != nil {
			http.Error(w, "read failed", 409)
			return
		}
		var probe bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&probe, gzip.BestSpeed)
		zw.Write(sample)
		zw.Close()
		compress = probe.Len()*8 <= len(sample)*7
	}
	if compress {
		w.Header().Set("Content-Encoding", "gzip")
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	}

	if _, n, _, _, err := EncodeBody(dst, input, fi.Name, compress, fi.Size); err == nil && n == fi.Size && s.OnServed != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		s.OnServed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), fi)
	}
}

// Check each chunk before writing: confirmation cannot revive an old fd request.
type versionWriter struct {
	progressWriter
	folder     *folder.Folder
	generation uint64
}

func (w *versionWriter) Write(p []byte) (int, error) {
	if err := w.folder.CheckRead(w.generation); err != nil {
		return 0, err
	}
	return w.progressWriter.Write(p)
}

type Client struct {
	// LocalHello supplies durable local epochs; set before concurrent use.
	LocalHello      func() Hello
	http            *http.Client
	port            int
	BodyIdleTimeout time.Duration
	IndexLimit      int64
}

// NewClient создаёт клиента, который доверяет только серверу с ID peer.
// port — порт нашего сервера, его узнает пир. relayDial открывает канал
// к пиру через relay, когда адрес — RelayHost (может быть nil).
func NewClient(id *identity.Identity, peer string, port int, relayDial func(context.Context) (net.Conn, error)) *Client {
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // цепочку не проверяем: проверяем ID ниже
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pid, err := peerID(raw)
			if err != nil {
				return err
			}
			if pid != peer {
				return fmt.Errorf("по адресу отвечает другое устройство (%s)", identity.ShortID(pid))
			}
			return nil
		},
	}
	c := &Client{port: port, http: &http.Client{Transport: &http.Transport{
		TLSClientConfig:       tlsCfg,
		ResponseHeaderTimeout: pollTimeout + 10*time.Second,
		DialContext:           dialer(relayDial),
		TLSHandshakeTimeout:   5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
	}}}
	c.http.Transport = &helloTransport{base: c.http.Transport, port: port, client: c}
	return c
}

// Negotiate before each operation, including after reconnect or address changes.
// No successful TLS connection is treated as protocol compatibility evidence.
type helloTransport struct {
	base   http.RoundTripper
	port   int
	client *Client
}

func (t *helloTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
func (t *helloTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	helloCtx, cancel := context.WithTimeout(r.Context(), BodyIdleTimeout)
	defer cancel()
	h := r.Clone(helloCtx)
	u := *r.URL
	u.Path = ProtocolHelloPath
	u.RawQuery = ""
	h.URL = &u
	h.Method = http.MethodPost
	local := Hello{Protocol: ProtocolVersion, Features: []string{"files", "gzip"}}
	if t.client != nil && t.client.LocalHello != nil {
		local = t.client.LocalHello()
	}
	local.Protocol = ProtocolVersion
	b, _ := json.Marshal(local)
	h.Body = io.NopCloser(bytes.NewReader(b))
	h.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	h.ContentLength = int64(len(b))
	h.Header = make(http.Header)
	h.Header.Set(ProtocolHeader, "2")
	h.Header.Set("Content-Type", "application/json")
	h.Header.Set(portHeader, strconv.Itoa(t.port))
	resp, err := t.base.RoundTrip(h)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		if resp.StatusCode == 503 {
			return nil, ErrPeerPaused
		}
		return nil, &ProtocolError{Expected: 2, Received: resp.Status}
	}
	_, err = DecodeHello(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set(ProtocolHeader, "2")
	return t.base.RoundTrip(r)
}

func dialer(relayDial func(context.Context) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	direct := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == RelayHost {
			if relayDial == nil {
				return nil, errors.New("relay is not configured")
			}
			return relayDial(ctx)
		}
		return direct(ctx, network, addr)
	}
}

func (c *Client) get(ctx context.Context, addr, path string, q url.Values) (*http.Response, error) {
	u := url.URL{Scheme: "https", Host: addr, Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(portHeader, strconv.Itoa(c.port))
	req.Header.Set(ProtocolHeader, "2")
	req.Header.Set("Accept-Encoding", "identity")
	if addr == RelayHost || strings.HasPrefix(addr, RelayHost+":") {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		resp.Body.Close()
		return nil, ErrPeerPaused
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}
	return resp, nil
}

// Index ждёт изменения индекса пира относительно since (long-poll).
func (c *Client) Index(ctx context.Context, addr string, since uint64) (*IndexResponse, error) {
	return c.IndexSince(ctx, addr, "", since, "")
}

// IndexSince preserves the peer epoch/cursor; name requests refresh only that record.
func (c *Client) IndexSince(ctx context.Context, addr, epoch string, since uint64, name string) (*IndexResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout+20*time.Second)
	defer cancel()
	resp, err := c.get(ctx, addr, "/index", url.Values{"since": {strconv.FormatUint(since, 10)}, "epoch": {epoch}, "name": {name}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := c.IndexLimit
	if limit <= 0 {
		limit = MaxIndexBytes
	}
	body := c.progressBody(resp.Body, cancel)
	var out IndexResponse
	if err := decodeIndex(&out, &indexLimitReader{r: body, limit: limit}); err != nil {
		return nil, err
	}
	return &out, nil
}

var errIndexTooLarge = errors.New("index exceeds size limit")

// indexLimitReader probes one byte past the limit so an exactly-sized body is
// accepted while an oversized one fails instead of appearing truncated.
type indexLimitReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (r *indexLimitReader) Read(p []byte) (int, error) {
	if r.n == r.limit {
		var probe [1]byte
		n, err := r.r.Read(probe[:])
		if n != 0 {
			return 0, errIndexTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.limit-r.n {
		p = p[:r.limit-r.n]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func decodeIndex(out *IndexResponse, r io.Reader) error {
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("invalid index: expected object")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("invalid index field")
		}
		switch strings.ToLower(name) {
		case "protocol":
			err = d.Decode(&out.Protocol)
		case "storeepoch":
			err = d.Decode(&out.StoreEpoch)
		case "reset":
			err = d.Decode(&out.Reset)
		case "cursor":
			err = d.Decode(&out.Cursor)
			out.Seq = out.Cursor
		case "seq":
			err = d.Decode(&out.Seq)
		case "files":
			out.Files, err = decodeFiles(d)
		default:
			var ignored json.RawMessage
			err = d.Decode(&ignored)
		}
		if err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	// Consume trailing whitespace and reject trailing values, also forcing the
	// size-limited reader to probe for an oversized body.
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("invalid index: trailing data")
		}
		return err
	}
	return nil
}

func decodeFiles(d *json.Decoder) ([]folder.FileInfo, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('[') {
		return nil, errors.New("invalid index: files must be an array")
	}
	files := make([]folder.FileInfo, 0)
	for d.More() {
		var fi folder.FileInfo
		if err := d.Decode(&fi); err != nil {
			return nil, err
		}
		if len(fi.Name) > 4096 || len(fi.Version) > 64 {
			return nil, invalidIndexRecord(fi.Name)
		}
		files = append(files, fi)
	}
	_, err = d.Token()
	return files, err
}

func invalidIndexRecord(name string) error {
	return fmt.Errorf("invalid index record %q: name or version exceeds limit", name[:min(len(name), 80)])
}

// Fetcher возвращает загрузчик файлов с пира по адресу addr.
func (c *Client) Fetcher(addr string) folder.Fetcher {
	return func(ctx context.Context, fi folder.FileInfo, w io.Writer) error {
		if fi.Size < 0 || fi.Size == int64(^uint64(0)>>1) {
			return errors.New("invalid file size")
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		resp, err := c.get(ctx, addr, "/file", url.Values{"name": {fi.Name}, "hash": {fi.Hash}})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		encoding := resp.Header.Get("Content-Encoding")
		if encoding == "" {
			encoding = "identity"
		}
		n, _, err := DecodeBody(w, c.progressBody(resp.Body, cancel), encoding, fi.Size)
		if err != nil {
			return err
		}
		if n != fi.Size {
			return fmt.Errorf("file size mismatch: got %d, expected %d", n, fi.Size)
		}
		return nil
	}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// Deadlines are refreshed per write; no total transfer duration limit.
type progressWriter struct {
	w    http.ResponseWriter
	idle time.Duration
}

func (s *Server) writeIdle() time.Duration {
	if s.WriteIdleTimeout > 0 {
		return s.WriteIdleTimeout
	}
	return BodyIdleTimeout
}
func (w *progressWriter) Write(p []byte) (int, error) {
	if err := http.NewResponseController(w.w).SetWriteDeadline(time.Now().Add(w.idle)); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

type progressReader struct {
	r      io.Reader
	cancel context.CancelFunc
	idle   time.Duration
}

func (c *Client) progressBody(r io.Reader, cancel context.CancelFunc) io.Reader {
	idle := c.BodyIdleTimeout
	if idle <= 0 {
		idle = BodyIdleTimeout
	}
	return &progressReader{r, cancel, idle}
}
func (r *progressReader) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.idle, r.cancel)
	defer timer.Stop()
	return r.r.Read(p)
}

// Connections is shared by direct TCP and relay listeners. Its mutex only
// protects connection bookkeeping; callbacks and network closes run outside it.
type Connections struct {
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	count      int
	relayCount int
	ips        map[string]int
	Max, PerIP int
}

func (g *Connections) State(c net.Conn, state http.ConnState) {
	g.mu.Lock()
	if g.conns == nil {
		g.conns = make(map[net.Conn]struct{})
	}
	// Only completed handshakes are inspected: ConnectionState waits for an
	// in-progress handshake. StateActive also closes the handshake/revocation race.
	if state == http.StateClosed || state == http.StateHijacked {
		delete(g.conns, c)
	} else if state == http.StateActive || state == http.StateIdle {
		g.conns[c] = struct{}{}
	}
	g.mu.Unlock()
}
func (g *Connections) Revoke(allowed func(string) bool) {
	g.mu.Lock()
	cs := make([]net.Conn, 0, len(g.conns))
	for c := range g.conns {
		cs = append(cs, c)
	}
	g.mu.Unlock()
	for _, c := range cs {
		if tc, ok := c.(*tls.Conn); ok {
			st := tc.ConnectionState()
			if len(st.PeerCertificates) > 0 && !allowed(identity.IDFromCert(st.PeerCertificates[0].Raw)) {
				c.Close()
			}
		}
	}
}

type limitedListener struct {
	net.Listener
	g        *Connections
	relayMax int
}
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error                      { err := c.Conn.Close(); c.once.Do(c.release); return err }
func (g *Connections) Limit(l net.Listener) net.Listener { return &limitedListener{Listener: l, g: g} }

// LimitRelay shares the global connection limit, with a separate relay-channel limit.
func (g *Connections) LimitRelay(l net.Listener, max int) net.Listener {
	if max <= 0 {
		max = 64
	}
	return &limitedListener{Listener: l, g: g, relayMax: max}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := ""
		if c.RemoteAddr().Network() == "tcp" {
			ip, _, _ = net.SplitHostPort(c.RemoteAddr().String())
		}
		g := l.g
		g.mu.Lock()
		total, per := g.Max, g.PerIP
		if total <= 0 {
			total = 128
		}
		if per <= 0 {
			per = 16
		}
		if g.ips == nil {
			g.ips = make(map[string]int)
		}
		if g.count >= total || (l.relayMax > 0 && g.relayCount >= l.relayMax) ||
			(l.relayMax == 0 && ip != "" && g.ips[ip] >= per) {
			g.mu.Unlock()
			c.Close()
			continue
		}
		g.count++
		if l.relayMax > 0 {
			g.relayCount++
		} else if ip != "" {
			g.ips[ip]++
		}
		g.mu.Unlock()
		return &limitedConn{Conn: c, release: func() {
			g.mu.Lock()
			g.count--
			if l.relayMax > 0 {
				g.relayCount--
			} else if ip != "" {
				g.ips[ip]--
				if g.ips[ip] == 0 {
					delete(g.ips, ip)
				}
			}
			g.mu.Unlock()
		}}, nil
	}
}
