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

func TestDeviceTableIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	d := &Discovery{devices: map[string]Device{}, now: func() time.Time { return now }}
	for i := 0; i < 10_000; i++ {
		id := identity.IDFromCert([]byte(strconv.Itoa(i)))
		d.remember(Announcement{ID: id, Name: "device"}, "127.0.0.1:1", d.now())
	}
	if got := len(d.devices); got > maxDevices {
		t.Fatalf("device table grew to %d, limit is %d", got, maxDevices)
	}
}

func TestDeviceTableKeepsPairedPeerDuringFlood(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	peerID := identity.IDFromCert([]byte("paired peer"))
	d := &Discovery{devices: map[string]Device{}, now: func() time.Time { return now }, keep: func(id string) bool { return id == peerID }}
	d.remember(Announcement{ID: peerID, Name: "paired"}, "192.0.2.1:1", now)
	for i := 0; i < maxDevices-1; i++ {
		id := identity.IDFromCert([]byte("fake " + strconv.Itoa(i)))
		d.remember(Announcement{ID: id}, "192.0.2.2:1", now)
	}
	for i := 0; i < 10_000; i++ {
		id := identity.IDFromCert([]byte("flood " + strconv.Itoa(i)))
		d.remember(Announcement{ID: id}, "192.0.2.3:1", now)
	}
	if _, ok := d.devices[peerID]; !ok {
		t.Fatal("paired peer was evicted by announcement flood")
	}
	if got := len(d.devices); got != maxDevices {
		t.Fatalf("device table has %d entries, want %d", got, maxDevices)
	}
}

func TestDeviceTableDoesNotAddWhenAllEntriesAreKept(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	d := &Discovery{devices: map[string]Device{}, now: func() time.Time { return now }, keep: func(string) bool { return true }}
	for i := 0; i < maxDevices; i++ {
		id := identity.IDFromCert([]byte("kept " + strconv.Itoa(i)))
		d.remember(Announcement{ID: id}, "192.0.2.1:1", now)
	}
	newID := identity.IDFromCert([]byte("new unknown"))
	d.remember(Announcement{ID: newID}, "192.0.2.2:1", now)
	if got := len(d.devices); got != maxDevices {
		t.Fatalf("device table has %d entries, want %d", got, maxDevices)
	}
	if _, ok := d.devices[newID]; ok {
		t.Fatal("unknown ID was added while every entry was kept")
	}
}

func TestDeviceTableStillEvictsOldestWithoutKeep(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	d := &Discovery{devices: map[string]Device{}, now: func() time.Time { return now }}
	var oldestID string
	for i := 0; i < maxDevices; i++ {
		id := identity.IDFromCert([]byte("device " + strconv.Itoa(i)))
		if i == 0 {
			oldestID = id
		}
		d.remember(Announcement{ID: id}, "192.0.2.1:1", now)
		now = now.Add(time.Millisecond)
	}
	newID := identity.IDFromCert([]byte("replacement"))
	d.remember(Announcement{ID: newID}, "192.0.2.2:1", now)
	if _, ok := d.devices[oldestID]; ok {
		t.Fatal("oldest device was not evicted without Keep")
	}
	if _, ok := d.devices[newID]; !ok {
		t.Fatal("new device was not added without Keep")
	}
}

func TestRememberRemovesExpiredDevicesAndRefreshesKnownDevice(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	d := &Discovery{devices: map[string]Device{}, now: func() time.Time { return now }}
	id := identity.IDFromCert([]byte("same peer"))
	d.remember(Announcement{ID: id, Name: "peer"}, "127.0.0.1:1", d.now())
	now = now.Add(time.Second)
	d.remember(Announcement{ID: id, Name: "peer updated"}, "127.0.0.1:2", d.now())
	if got := len(d.devices); got != 1 {
		t.Fatalf("repeat announcement created another entry: %d", got)
	}
	if got := d.devices[id]; got.Name != "peer updated" || got.Addr != "127.0.0.1:2" {
		t.Fatalf("known device was not updated: %+v", got)
	}
	now = now.Add(ttl + 2*time.Second)
	d.remember(Announcement{ID: identity.IDFromCert([]byte("new peer")), Name: "new"}, "127.0.0.1:3", d.now())
	if got := len(d.devices); got != 1 {
		t.Fatalf("expired device was retained: %d entries", got)
	}
}

func TestAnnouncementRateLimitPerIP(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	d := &Discovery{ipRates: map[string]rateWindowCounter{}, now: func() time.Time { return now }}
	for i := 0; i < perIPRateLimit; i++ {
		if !d.allowAnnouncement("192.0.2.1", d.now()) {
			t.Fatalf("announcement %d was rejected before per-IP limit", i+1)
		}
	}
	if d.allowAnnouncement("192.0.2.1", d.now()) {
		t.Fatal("announcement over per-IP limit was accepted")
	}
	if !d.allowAnnouncement("192.0.2.2", d.now()) {
		t.Fatal("one source IP consumed another IP's allowance")
	}
	now = now.Add(rateWindow)
	if !d.allowAnnouncement("192.0.2.1", d.now()) {
		t.Fatal("per-IP limit did not reset after one second")
	}
}
