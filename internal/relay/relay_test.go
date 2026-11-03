package relay

import (
	"context"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"sharedir/internal/identity"
)

func newID(t *testing.T) *identity.Identity {
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func startRelay(t *testing.T) Endpoint {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &Server{ID: newID(t), Token: NewToken(), Log: log.New(io.Discard, "", 0)}
	go s.Serve(ln)
	return Endpoint{Addr: ln.Addr().String(), RelayID: s.ID.ID, Token: s.Token}
}

func TestRelayPipesBothWays(t *testing.T) {
	ep := startRelay(t)
	a := &Client{Endpoint: ep, ID: newID(t)}
	b := &Client{Endpoint: ep, ID: newID(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	go b.Listen(ctx, func() { close(ready) }, func(c net.Conn) {
		defer c.Close()
		io.Copy(c, c) // эхо
	})
	<-ready

	conn, err := a.Connect(ctx, b.ID.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := strings.Repeat("sharedir ", 50000) // больше одной TLS-записи
	go conn.Write([]byte(msg))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatal("эхо не совпало")
	}
}

func TestRelayRejects(t *testing.T) {
	ep := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bad := ep
	bad.Token = "wrong"
	if _, err := (&Client{Endpoint: bad, ID: newID(t)}).Connect(ctx, newID(t).ID); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("неверный токен: %v", err)
	}

	impostor := ep
	impostor.RelayID = newID(t).ID
	if _, err := (&Client{Endpoint: impostor, ID: newID(t)}).Connect(ctx, newID(t).ID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("чужой relay: %v", err)
	}

	if _, err := (&Client{Endpoint: ep, ID: newID(t)}).Connect(ctx, newID(t).ID); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("пир не в сети: %v", err)
	}
}

func TestParseURL(t *testing.T) {
	id := newID(t).ID
	ep, err := ParseURL(Scheme + "://" + identity.Pretty(id) + ":tok@202.148.53.107")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Addr != "202.148.53.107:"+DefaultPort || ep.RelayID != id || ep.Token != "tok" {
		t.Errorf("%+v", ep)
	}
	if _, err := ParseURL("https://x@y"); err == nil {
		t.Error("чужая схема принята")
	}
}

func TestRelayPreAuthLimit(t *testing.T) {
	ep := startRelay(t)
	// Молчащие соединения сверх лимита relay закрывает сразу.
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for range maxPreAuthPerIP {
		c, err := net.Dial("tcp", ep.Addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(200 * time.Millisecond) // дать relay принять их
	extra, err := net.Dial("tcp", ep.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("лишнее соединение не закрыто: %v", err)
	}

	// Авторизованный клиент лимит не занимает: после закрытия молчащих
	// соединений обычное подключение проходит.
	for _, c := range conns {
		c.Close()
	}
	conns = nil
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (&Client{Endpoint: ep, ID: newID(t)}).Connect(ctx, newID(t).ID); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("после освобождения лимита: %v", err)
	}
}
