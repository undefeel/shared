package relay

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"sharedir/internal/identity"
)

const (
	helloTimeout      = 10 * time.Second
	acceptTimeout     = 15 * time.Second
	pingInterval      = 30 * time.Second
	maxPending        = 64 // одновременных незавершённых connect на устройство
	maxListeners      = 128
	maxListenersPerIP = 32
	maxTunnels        = 64
	maxTunnelsPerID   = 8
	tunnelIdleTimeout = 5 * time.Minute
	tunnelIdleCheck   = 15 * time.Second

	// Лимиты на соединения, ещё не предъявившие токен: иначе посторонний
	// может держать тысячи молчащих соединений. Лимит именно до проверки
	// токена — свои устройства через VPN часто приходят с одного IP.
	maxPreAuthPerIP = 32
	maxPreAuth      = 1024
)

// Server — сам relay. Ничего не хранит на диске, кроме своего ключа:
// только таблица «ID устройства → его управляющий канал» в памяти.
type Server struct {
	ID    *identity.Identity
	Token string
	Log   *log.Logger

	mu          sync.Mutex
	listeners   map[string]*control
	pending     map[string]*pendingConn
	preAuth     map[string]int // IP → соединений до проверки токена
	preAuthN    int            // всего таких
	tunnels     int            // зарезервированные и активные pipe
	tunnelsByID map[string]int
	badLogged   map[string]time.Time // IP → когда последний раз писали в лог
}

func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

// enterPreAuth учитывает соединение до проверки токена; false — лимит исчерпан.
func (s *Server) enterPreAuth(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preAuth[ip] >= maxPreAuthPerIP || s.preAuthN >= maxPreAuth {
		return false
	}
	s.preAuth[ip]++
	s.preAuthN++
	return true
}

func (s *Server) leavePreAuth(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preAuth[ip]--; s.preAuth[ip] <= 0 {
		delete(s.preAuth, ip)
	}
	s.preAuthN--
}

// logBadToken пишет о неверном токене не чаще раза в минуту на IP.
func (s *Server) logBadToken(ip, id string) {
	s.mu.Lock()
	last, ok := s.badLogged[ip]
	write := !ok || time.Since(last) > time.Minute
	if write {
		if len(s.badLogged) > 10000 {
			s.badLogged = map[string]time.Time{} // не даём таблице расти бесконечно
		}
		s.badLogged[ip] = time.Now()
	}
	s.mu.Unlock()
	if write {
		s.Log.Printf("%s %s: неверный токен", ip, identity.ShortID(id))
	}
}

type control struct {
	conn net.Conn
	ip   string
	wmu  sync.Mutex
}

func (c *control) send(m Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return writeJSON(c.conn, m)
}

type pendingConn struct {
	from, to string
	ch       chan net.Conn
	state    pendingState
}

type pendingState uint8

const (
	pendingWaiting pendingState = iota
	pendingAccepted
	pendingExpired
)

func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.ID.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no client certificate")
			}
			return nil
		},
	}
}

func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.listeners = map[string]*control{}
	s.pending = map[string]*pendingConn{}
	s.preAuth = map[string]int{}
	s.tunnelsByID = map[string]int{}
	s.badLogged = map[string]time.Time{}
	s.mu.Unlock()
	tln := tls.NewListener(ln, s.tlsConfig())
	for {
		c, err := tln.Accept()
		if err != nil {
			return err
		}
		ip := remoteIP(c)
		if !s.enterPreAuth(ip) {
			c.Close()
			continue
		}
		go s.handle(c.(*tls.Conn), ip)
	}
}

func (s *Server) handle(conn *tls.Conn, ip string) {
	keep := false
	authed := false
	defer func() {
		if !authed {
			s.leavePreAuth(ip)
		}
		if !keep {
			conn.Close()
		}
	}()
	conn.SetDeadline(time.Now().Add(helloTimeout))
	if err := conn.Handshake(); err != nil {
		return
	}
	id := identity.IDFromCert(conn.ConnectionState().PeerCertificates[0].Raw)
	var h Hello
	if err := readJSON(conn, &h); err != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(h.Token), []byte(s.Token)) != 1 {
		s.logBadToken(ip, id)
		writeJSON(conn, Message{Error: "bad token"})
		return
	}
	authed = true
	s.leavePreAuth(ip)
	conn.SetDeadline(time.Time{})

	switch h.Op {
	case "listen":
		s.serveListen(conn, id)
	case "connect":
		keep = s.serveConnect(conn, id, h.To)
	case "accept":
		keep = s.serveAccept(conn, id, h.Session)
	default:
		writeJSON(conn, Message{Error: "unknown op"})
	}
}

// serveListen держит управляющий канал устройства, пока тот жив.
func (s *Server) serveListen(conn *tls.Conn, id string) {
	c := &control{conn: conn, ip: remoteIP(conn)}
	s.mu.Lock()
	old := s.listeners[id]
	countIP := 0
	for _, listener := range s.listeners {
		if listener.ip == c.ip {
			countIP++
		}
	}
	if old != nil && old.ip == c.ip {
		countIP--
	}
	if (old == nil && len(s.listeners) >= maxListeners) || countIP >= maxListenersPerIP {
		s.mu.Unlock()
		writeJSON(conn, Message{Error: "too many listeners"})
		return
	}
	s.listeners[id] = c
	n := len(s.listeners)
	s.mu.Unlock()
	if old != nil {
		old.conn.Close() // переподключение того же устройства
	}
	s.Log.Printf("устройство %s на связи (%s), всего: %d", identity.ShortID(id), conn.RemoteAddr(), n)

	defer func() {
		s.mu.Lock()
		if s.listeners[id] == c {
			delete(s.listeners, id)
		}
		s.mu.Unlock()
		s.Log.Printf("устройство %s отключилось", identity.ShortID(id))
	}()

	if c.send(Message{OK: true}) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		// Клиент сюда ничего не пишет: чтение нужно, чтобы заметить обрыв.
		io.Copy(io.Discard, conn)
		close(done)
	}()
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if c.send(Message{Op: "ping"}) != nil {
				return
			}
		}
	}
}

func (s *Server) serveConnect(conn *tls.Conn, from, to string) bool {
	to, err := identity.Normalize(to)
	if err != nil {
		writeJSON(conn, Message{Error: "invalid device ID"})
		return false
	}
	s.mu.Lock()
	target := s.listeners[to]
	busy := 0
	for _, p := range s.pending {
		if p.from == from {
			busy++
		}
	}
	session := NewToken()
	p := &pendingConn{from: from, to: to, ch: make(chan net.Conn, 1)}
	if target != nil && busy < maxPending {
		s.pending[session] = p
	}
	s.mu.Unlock()
	if target == nil {
		writeJSON(conn, Message{Error: "offline"})
		return false
	}
	if busy >= maxPending {
		writeJSON(conn, Message{Error: "too many pending connections"})
		return false
	}
	defer s.expirePending(session, p)

	if target.send(Message{Op: "incoming", From: from, Session: session}) != nil {
		writeJSON(conn, Message{Error: "offline"})
		return false
	}
	var peer net.Conn
	select {
	case peer = <-p.ch:
	case <-time.After(acceptTimeout):
		s.expirePending(session, p)
		writeJSON(conn, Message{Error: "peer did not answer"})
		return false
	}
	if !s.reserveTunnel(from) {
		peer.Close()
		writeJSON(conn, Message{Error: "too many tunnels"})
		return false
	}
	if writeJSON(peer, Message{OK: true}) != nil || writeJSON(conn, Message{OK: true}) != nil {
		peer.Close()
		s.releaseTunnel(from)
		return false
	}
	go func() {
		defer s.releaseTunnel(from)
		pipe(conn, peer)
	}()
	return true
}

// expirePending serializes timeout cleanup with accept. If accept already queued
// its socket, timeout owns and closes it; a later accept sees expired state.
func (s *Server) expirePending(session string, p *pendingConn) {
	s.mu.Lock()
	if p.state == pendingWaiting || p.state == pendingAccepted {
		p.state = pendingExpired
		delete(s.pending, session)
		select {
		case peer := <-p.ch:
			peer.Close()
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Server) reserveTunnel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnels >= maxTunnels || s.tunnelsByID[id] >= maxTunnelsPerID {
		return false
	}
	s.tunnels++
	s.tunnelsByID[id]++
	return true
}

func (s *Server) releaseTunnel(id string) {
	s.mu.Lock()
	s.tunnels--
	if s.tunnelsByID[id]--; s.tunnelsByID[id] == 0 {
		delete(s.tunnelsByID, id)
	}
	s.mu.Unlock()
}

func (s *Server) serveAccept(conn *tls.Conn, id, session string) bool {
	if !s.acceptPending(session, id, conn) {
		writeJSON(conn, Message{Error: "unknown session"})
		return false
	}
	return true
}

func (s *Server) acceptPending(session, id string, conn net.Conn) bool {
	s.mu.Lock()
	p := s.pending[session]
	if p == nil || p.to != id || p.state != pendingWaiting {
		p = nil
	} else {
		p.state = pendingAccepted
		p.ch <- conn
	}
	s.mu.Unlock()
	return p != nil
}

// pipe гоняет байты в обе стороны, пока одна не закроется.
func pipe(a, b net.Conn) {
	activity := &pipeActivity{}
	activity.touch()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(activityWriter{Conn: dst, activity: activity}, activityReader{Conn: src, activity: activity})
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	idle := time.NewTicker(tunnelIdleCheck)
	defer idle.Stop()
	for {
		select {
		case <-done:
			goto closed
		case <-idle.C:
			if time.Since(time.Unix(0, activity.last.Load())) > tunnelIdleTimeout {
				goto closed
			}
		}
	}
closed:
	a.Close()
	b.Close()
	<-done
}

type pipeActivity struct{ last atomic.Int64 }

func (a *pipeActivity) touch() { a.last.Store(time.Now().UnixNano()) }

type activityReader struct {
	net.Conn
	activity *pipeActivity
}

func (c activityReader) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

type activityWriter struct {
	net.Conn
	activity *pipeActivity
}

func (c activityWriter) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}
