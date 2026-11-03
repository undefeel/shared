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
	"time"

	"sharedir/internal/identity"
)

const (
	helloTimeout  = 15 * time.Second
	acceptTimeout = 15 * time.Second
	pingInterval  = 30 * time.Second
	maxPending    = 64 // одновременных незавершённых connect на устройство
)

// Server — сам relay. Ничего не хранит на диске, кроме своего ключа:
// только таблица «ID устройства → его управляющий канал» в памяти.
type Server struct {
	ID    *identity.Identity
	Token string
	Log   *log.Logger

	mu        sync.Mutex
	listeners map[string]*control
	pending   map[string]*pendingConn
}

type control struct {
	conn net.Conn
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
}

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
	s.mu.Unlock()
	tln := tls.NewListener(ln, s.tlsConfig())
	for {
		c, err := tln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c.(*tls.Conn))
	}
}

func (s *Server) handle(conn *tls.Conn) {
	keep := false
	defer func() {
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
		s.Log.Printf("%s %s: неверный токен", conn.RemoteAddr(), identity.ShortID(id))
		writeJSON(conn, Message{Error: "bad token"})
		return
	}
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
	c := &control{conn: conn}
	s.mu.Lock()
	if old := s.listeners[id]; old != nil {
		old.conn.Close() // переподключение того же устройства
	}
	s.listeners[id] = c
	n := len(s.listeners)
	s.mu.Unlock()
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
	defer func() {
		s.mu.Lock()
		delete(s.pending, session)
		s.mu.Unlock()
	}()

	if target.send(Message{Op: "incoming", From: from, Session: session}) != nil {
		writeJSON(conn, Message{Error: "offline"})
		return false
	}
	var peer net.Conn
	select {
	case peer = <-p.ch:
	case <-time.After(acceptTimeout):
		writeJSON(conn, Message{Error: "peer did not answer"})
		return false
	}
	if writeJSON(peer, Message{OK: true}) != nil || writeJSON(conn, Message{OK: true}) != nil {
		peer.Close()
		return false
	}
	go pipe(conn, peer)
	return true
}

func (s *Server) serveAccept(conn *tls.Conn, id, session string) bool {
	s.mu.Lock()
	p := s.pending[session]
	if p != nil && p.to == id {
		delete(s.pending, session) // сессия одноразовая
	} else {
		p = nil
	}
	s.mu.Unlock()
	if p == nil {
		writeJSON(conn, Message{Error: "unknown session"})
		return false
	}
	p.ch <- conn
	return true
}

// pipe гоняет байты в обе стороны, пока одна не закроется.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}
