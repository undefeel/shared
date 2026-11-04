package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"sharedir/internal/identity"
)

// Сосед на 127.0.0.2 шлёт анонсы discovery на 127.0.0.1 и ждёт ответа.
// Широковещание не нужно: ответ уходит на src.IP:port.
func TestAskGetsImmediateReply(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("127.0.0.2 есть по умолчанию только в Linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	selfID, peerID := identity.IDFromCert([]byte("self")), identity.IDFromCert([]byte("peer"))
	seen := make(chan Device, 4)
	_, err = Start(ctx, port, Announcement{ID: selfID, Name: "self", Port: 1}, log.New(io.Discard, "", 0),
		func(dev Device) { seen <- dev })
	if err != nil {
		t.Fatal(err)
	}

	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) })
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort("127.0.0.2", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	peer := pc.(*net.UDPConn)
	defer peer.Close()

	send := func(ask bool) {
		b, _ := json.Marshal(Announcement{Magic: magic, ID: peerID, Name: "peer", Port: 2, Ask: ask})
		if _, err := peer.WriteToUDP(b, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	replied := func() bool {
		peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 2048)
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			return false
		}
		var a Announcement
		json.Unmarshal(buf[:n], &a)
		if a.ID != selfID || a.Ask {
			t.Fatalf("неожиданный ответ: %+v", a)
		}
		return true
	}

	// Мусор с коротким ID раньше ронял демон (a.ID[:7]); теперь — молча мимо.
	junk, _ := json.Marshal(Announcement{Magic: magic, ID: "X", Port: 2})
	peer.WriteToUDP(junk, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if replied() {
		t.Fatal("ответили на анонс с невалидным ID")
	}

	send(false)
	if !replied() {
		t.Fatal("новому соседу не ответили")
	}
	select {
	case dev := <-seen:
		if dev.ID != peerID || dev.Addr != "127.0.0.2:2" {
			t.Fatalf("onSeen: %+v", dev)
		}
	case <-time.After(time.Second):
		t.Fatal("onSeen не вызван")
	}

	send(false)
	if replied() {
		t.Fatal("знакомому соседу без Ask ответили — лишний трафик")
	}
	send(true)
	if !replied() {
		t.Fatal("на Ask не ответили сразу")
	}
	select {
	case dev := <-seen:
		t.Fatalf("onSeen повторно для того же адреса: %+v", dev)
	default:
	}
}
