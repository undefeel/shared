package app

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"sharedir/internal/relay"
	"sharedir/internal/transport"
)

// RelayStatus — состояние подключения к relay для UI.
type RelayStatus struct {
	Addr      string `json:"addr"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// relayAddr помечает соединения, пришедшие через relay: у них нет
// осмысленного удалённого адреса, и запоминать его как адрес пира нельзя.
type relayAddr struct{}

func (relayAddr) Network() string { return "relay" }
func (relayAddr) String() string  { return transport.RelayHost }

type relayConn struct{ net.Conn }

func (relayConn) RemoteAddr() net.Addr { return relayAddr{} }

// connListener — net.Listener, в который relay-клиент складывает входящие
// каналы от пиров; на нём работает тот же HTTP-сервер, что и на TCP.
type connListener struct {
	ch   chan net.Conn
	once sync.Once
	done chan struct{}
}

func newConnListener() *connListener {
	return &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *connListener) Addr() net.Addr { return relayAddr{} }

func (l *connListener) push(c net.Conn) {
	select {
	case l.ch <- relayConn{c}:
	case <-l.done:
		c.Close()
	}
}

// relayClient возвращает клиента relay по текущему конфигу или nil.
func (d *Daemon) relayClient() *relay.Client {
	d.mu.Lock()
	url := d.cfg.Relay
	d.mu.Unlock()
	if url == "" {
		return nil
	}
	ep, err := relay.ParseURL(url)
	if err != nil {
		return nil
	}
	return &relay.Client{Endpoint: ep, ID: d.ID}
}

// relayDial — дозвон до пира через relay (для transport.Client).
func (d *Daemon) relayDial(peer string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		c := d.relayClient()
		if c == nil {
			return nil, errors.New("relay is not configured")
		}
		return c.Connect(ctx, peer)
	}
}

func (d *Daemon) setRelayStatus(fn func(*RelayStatus)) {
	d.mu.Lock()
	if d.relaySt == nil {
		d.relaySt = &RelayStatus{}
	}
	fn(d.relaySt)
	d.mu.Unlock()
	d.changed()
}

// relayLoop держит регистрацию на relay, чтобы пиры могли дозвониться к нам.
// Перезапускается при смене адреса relay (Reload дёргает relayKick).
func (d *Daemon) relayLoop(ctx context.Context, ln *connListener) {
	backoff := time.Second
	for ctx.Err() == nil {
		c := d.relayClient()
		if c == nil {
			d.mu.Lock()
			d.relaySt = nil
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-d.relayKick:
			}
			continue
		}
		addr := c.Endpoint.Addr
		d.setRelayStatus(func(s *RelayStatus) { *s = RelayStatus{Addr: addr} })

		lctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-d.relayKick:
				cancel()
			case <-lctx.Done():
			}
		}()
		err := c.Listen(lctx, func() {
			backoff = time.Second
			d.Log.Printf("relay %s: на связи", addr)
			d.setRelayStatus(func(s *RelayStatus) { s.Connected, s.Error = true, "" })
		}, ln.push)
		reconfigured := lctx.Err() != nil && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil {
			return
		}
		d.setRelayStatus(func(s *RelayStatus) {
			s.Connected = false
			if err != nil && !reconfigured {
				s.Error = err.Error()
			}
		})
		if reconfigured {
			continue
		}
		d.Log.Printf("relay %s: %v, повтор через %s", addr, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-d.relayKick:
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}
