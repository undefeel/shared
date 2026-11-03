package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"sharedir/internal/identity"
)

// Client подключает устройство к relay.
type Client struct {
	Endpoint Endpoint
	ID       *identity.Identity
}

func (c *Client) dial(ctx context.Context) (*tls.Conn, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		Config: &tls.Config{
			Certificates:       []tls.Certificate{c.ID.Cert},
			InsecureSkipVerify: true, // проверяем ID relay ниже
			MinVersion:         tls.VersionTLS13,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || identity.IDFromCert(raw[0]) != c.Endpoint.RelayID {
					return errors.New("relay certificate does not match the relay address")
				}
				return nil
			},
		},
	}
	conn, err := d.DialContext(ctx, "tcp", c.Endpoint.Addr)
	if err != nil {
		return nil, err
	}
	return conn.(*tls.Conn), nil
}

// open отправляет Hello и ждёт ответа relay.
func (c *Client) open(ctx context.Context, h Hello) (*tls.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	h.Token = c.Endpoint.Token
	conn.SetDeadline(time.Now().Add(acceptTimeout + 5*time.Second))
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	var m Message
	if err = writeJSON(conn, h); err == nil {
		err = readJSON(conn, &m)
	}
	if err == nil && !m.OK {
		err = fmt.Errorf("relay: %s", m.Error)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// Connect открывает канал к устройству peer. Дальше по нему пиры говорят
// своим mTLS, как по обычному TCP.
func (c *Client) Connect(ctx context.Context, peer string) (net.Conn, error) {
	return c.open(ctx, Hello{Op: "connect", To: peer})
}

// Listen держит управляющий канал и для каждого входящего соединения
// вызывает accept с готовым каналом к пиру. Возвращается при обрыве.
// onReady вызывается, когда relay принял регистрацию.
func (c *Client) Listen(ctx context.Context, onReady func(), accept func(net.Conn)) error {
	conn, err := c.open(ctx, Hello{Op: "listen"})
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	if onReady != nil {
		onReady()
	}
	for {
		// relay пингует раз в 30 с; тишина дольше — соединение мертво.
		conn.SetReadDeadline(time.Now().Add(3 * pingInterval))
		var m Message
		if err := readJSON(conn, &m); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if m.Op == "incoming" {
			go func(session string) {
				actx, cancel := context.WithTimeout(ctx, acceptTimeout)
				defer cancel()
				if pc, err := c.open(actx, Hello{Op: "accept", Session: session}); err == nil {
					accept(pc)
				}
			}(m.Session)
		}
	}
}
