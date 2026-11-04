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

	"sharedir/internal/identity"
)

const (
	magic           = "sharedir/1"
	interval        = 30 * time.Second
	ttl             = 3 * interval
	maxDevices      = 256
	globalRateLimit = 1000 // announcements per second
	perIPRateLimit  = 10   // announcements per second
	maxRateLimitIPs = 4096
	rateWindow      = time.Second
)

type Announcement struct {
	Magic string `json:"m"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Port  int    `json:"port"`
	// Ask — «ответьте сразу»: шлётся при старте, чтобы найти соседей за
	// секунду, а не ждать их планового анонса (до interval). Старые версии
	// поле игнорируют.
	Ask bool `json:"ask,omitempty"`
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
	// onSeen вызывается, когда устройство появилось в сети: впервые, после
	// пропажи дольше ttl или на новом адресе. Вызывается вне блокировки.
	onSeen func(Device)
	keep   func(string) bool // immutable after StartWithKeep starts the receiver

	mu         sync.Mutex
	devices    map[string]Device
	rateMu     sync.Mutex
	now        func() time.Time
	globalRate rateWindowCounter
	ipRates    map[string]rateWindowCounter
}

type rateWindowCounter struct {
	start time.Time
	count int
}

func Start(ctx context.Context, port int, self Announcement, logger *log.Logger, onSeen func(Device)) (*Discovery, error) {
	return StartWithKeep(ctx, port, self, logger, onSeen, nil)
}

// StartWithKeep starts discovery with an optional predicate for entries that
// must not be evicted when the device table is full. The predicate runs
// without d.mu, so it may safely acquire locks owned by its caller.
func StartWithKeep(ctx context.Context, port int, self Announcement, logger *log.Logger, onSeen func(Device), keep func(string) bool) (*Discovery, error) {
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
	d := &Discovery{conn: pc.(*net.UDPConn), port: port, self: self, log: logger, onSeen: onSeen, keep: keep, devices: map[string]Device{}, now: time.Now, ipRates: map[string]rateWindowCounter{}}
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

func (d *Discovery) packet(ask bool) []byte {
	a := d.self
	a.Ask = ask
	b, _ := json.Marshal(a)
	return b
}

func (d *Discovery) announce(ask bool) {
	pkt := d.packet(ask)
	for _, ip := range broadcastAddrs() {
		d.conn.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: d.port})
	}
}

func (d *Discovery) sendLoop(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	ask := true
	for {
		d.announce(ask)
		ask = false
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
		if json.Unmarshal(buf[:n], &a) != nil || a.Magic != magic || a.ID == d.self.ID || a.Port <= 0 || a.Port > 65535 {
			continue
		}
		// Пакет может прислать кто угодно в сети: принимаем только канонический ID.
		if id, err := identity.Normalize(a.ID); err != nil || id != a.ID {
			continue
		}
		now := d.now()
		if !d.allowAnnouncement(src.IP.String(), now) {
			continue
		}
		addr := net.JoinHostPort(src.IP.String(), strconv.Itoa(a.Port))
		dev, prev, known := d.remember(a, addr, now)
		if !known || prev.Addr != addr {
			d.log.Printf("обнаружено устройство %q (%s) на %s", a.Name, identity.ShortID(a.ID), addr)
		}
		if d.onSeen != nil && (!known || prev.Addr != addr || now.Sub(prev.LastSeen) > ttl) {
			d.onSeen(dev)
		}
		// Новому соседу или тому, кто просит, отвечаем сразу, не дожидаясь
		// своего таймера. Ответ без Ask, так что пинг-понга не будет.
		if a.Ask || !known || now.Sub(prev.LastSeen) > interval {
			d.conn.WriteToUDP(d.packet(false), &net.UDPAddr{IP: src.IP, Port: d.port})
		}
	}
}

func (d *Discovery) remember(a Announcement, addr string, now time.Time) (Device, Device, bool) {
	d.mu.Lock()
	d.removeExpiredLocked(now)
	prev, known := d.devices[a.ID]
	dev := Device{ID: a.ID, Name: a.Name, Addr: addr, LastSeen: now}
	if known || len(d.devices) < maxDevices {
		d.devices[a.ID] = dev
		d.mu.Unlock()
		return dev, prev, known
	}
	keep := d.keep
	candidates := make(map[string]time.Time, len(d.devices))
	for id, existing := range d.devices {
		candidates[id] = existing.LastSeen
	}
	d.mu.Unlock()

	// Keep may acquire daemon.mu; never call it while holding discovery.mu.
	if keep != nil {
		for id := range candidates {
			if keep(id) {
				delete(candidates, id)
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeExpiredLocked(now)
	prev, known = d.devices[a.ID]
	if known {
		d.devices[a.ID] = dev
		return dev, prev, true
	}
	if len(d.devices) >= maxDevices {
		var oldestID string
		var oldest time.Time
		for id, seen := range candidates {
			if current, exists := d.devices[id]; exists && current.LastSeen.Equal(seen) && (oldestID == "" || seen.Before(oldest)) {
				oldestID, oldest = id, seen
			}
		}
		if oldestID == "" {
			return dev, Device{}, false
		}
		delete(d.devices, oldestID)
	}
	d.devices[a.ID] = dev
	return dev, prev, false
}

// allowAnnouncement applies one global and one per-source-IP fixed-window limit.
// Expired source entries are pruned here so rate accounting also stays bounded.
func (d *Discovery) allowAnnouncement(ip string, now time.Time) bool {
	d.rateMu.Lock()
	defer d.rateMu.Unlock()
	d.globalRate = advanceRateWindow(d.globalRate, now)
	for source, counter := range d.ipRates {
		if rateWindowExpired(counter, now) {
			delete(d.ipRates, source)
		}
	}
	counter, exists := d.ipRates[ip]
	if !exists && len(d.ipRates) >= maxRateLimitIPs {
		return false
	}
	counter = advanceRateWindow(counter, now)
	if d.globalRate.count >= globalRateLimit || counter.count >= perIPRateLimit {
		return false
	}
	d.globalRate.count++
	counter.count++
	d.ipRates[ip] = counter
	return true
}

func advanceRateWindow(counter rateWindowCounter, now time.Time) rateWindowCounter {
	if counter.start.IsZero() || now.Sub(counter.start) >= rateWindow || now.Before(counter.start) {
		return rateWindowCounter{start: now}
	}
	return counter
}

func rateWindowExpired(counter rateWindowCounter, now time.Time) bool {
	return !counter.start.IsZero() && (now.Sub(counter.start) >= rateWindow || now.Before(counter.start))
}

func (d *Discovery) removeExpiredLocked(now time.Time) {
	for id, dev := range d.devices {
		if now.Sub(dev.LastSeen) > ttl || now.Before(dev.LastSeen) {
			delete(d.devices, id)
		}
	}
}

// Lookup возвращает свежий адрес устройства, если оно недавно анонсировалось.
func (d *Discovery) Lookup(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.removeExpiredLocked(now)
	dev, ok := d.devices[id]
	if !ok {
		return "", false
	}
	return dev.Addr, true
}

func (d *Discovery) Devices() []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.removeExpiredLocked(now)
	var out []Device
	for _, dev := range d.devices {
		out = append(out, dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
