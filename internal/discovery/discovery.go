// Package discovery находит другие экземпляры sharedir в локальной сети
// UDP-броадкастом. Анонс содержит только ID, имя и порт; доверие всё равно
// проверяется по сертификату при подключении, так что подделка анонса
// максимум направит подключение не туда, и оно не пройдёт проверку ID.
package discovery

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	magic    = "sharedir/1"
	interval = 30 * time.Second
	ttl      = 3 * interval
)

type Announcement struct {
	Magic string `json:"m"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Port  int    `json:"port"`
}

type Device struct {
	ID       string
	Name     string
	Addr     string // host:port TCP-сервера
	LastSeen time.Time
}

type Discovery struct {
	conn *net.UDPConn
	port int
	self Announcement
	log  *log.Logger

	mu      sync.Mutex
	devices map[string]Device
}

func Start(ctx context.Context, port int, self Announcement, logger *log.Logger) (*Discovery, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			// REUSEPORT позволяет нескольким экземплярам на одной машине
			// слушать один порт (броадкаст получат все).
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
			if serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			}
		})
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	self.Magic = magic
	d := &Discovery{conn: pc.(*net.UDPConn), port: port, self: self, log: logger, devices: map[string]Device{}}
	go d.recvLoop()
	go d.sendLoop(ctx)
	go func() { <-ctx.Done(); d.conn.Close() }()
	return d, nil
}

// broadcastAddrs — широковещательные адреса всех поднятых IPv4-интерфейсов.
// Шлём на каждый явно: на macOS 255.255.255.255 уходит только через
// интерфейс маршрута по умолчанию, а его может перехватить VPN.
func broadcastAddrs() []net.IP {
	out := []net.IP{net.IPv4bcast}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			mask := net.IP(ipn.Mask).To4()
			if mask == nil {
				continue
			}
			bc := make(net.IP, 4)
			for i := range bc {
				bc[i] = ip[i] | ^mask[i]
			}
			out = append(out, bc)
		}
	}
	return out
}

func (d *Discovery) packet() []byte {
	b, _ := json.Marshal(d.self)
	return b
}

func (d *Discovery) announce() {
	pkt := d.packet()
	for _, ip := range broadcastAddrs() {
		d.conn.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: d.port})
	}
}

func (d *Discovery) sendLoop(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.announce()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Discovery) recvLoop() {
	buf := make([]byte, 2048)
	for {
		n, src, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var a Announcement
		if json.Unmarshal(buf[:n], &a) != nil || a.Magic != magic || a.ID == d.self.ID || a.Port <= 0 {
			continue
		}
		addr := net.JoinHostPort(src.IP.String(), strconv.Itoa(a.Port))
		d.mu.Lock()
		prev, known := d.devices[a.ID]
		d.devices[a.ID] = Device{ID: a.ID, Name: a.Name, Addr: addr, LastSeen: time.Now()}
		d.mu.Unlock()
		if !known || prev.Addr != addr {
			d.log.Printf("обнаружено устройство %q (%s) на %s", a.Name, a.ID[:7], addr)
		}
		// Новому соседу отвечаем сразу, не дожидаясь своего таймера.
		if !known || time.Since(prev.LastSeen) > interval {
			d.conn.WriteToUDP(d.packet(), &net.UDPAddr{IP: src.IP, Port: d.port})
		}
	}
}

// Lookup возвращает свежий адрес устройства, если оно недавно анонсировалось.
func (d *Discovery) Lookup(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dev, ok := d.devices[id]
	if !ok || time.Since(dev.LastSeen) > ttl {
		return "", false
	}
	return dev.Addr, true
}

func (d *Discovery) Devices() []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Device
	for _, dev := range d.devices {
		if time.Since(dev.LastSeen) <= ttl {
			out = append(out, dev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
