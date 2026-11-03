// Package app собирает демон: папку, сервер, клиентов пиров, обнаружение и
// управляющий сокет.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/discovery"
	"sharedir/internal/folder"
	"sharedir/internal/identity"
	"sharedir/internal/transport"
)

const rescanInterval = time.Minute

type PeerStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Addr      string    `json:"addr,omitempty"`
	State     string    `json:"state"`
	LastSync  time.Time `json:"last_sync,omitzero"`
}

type Candidate struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Addr   string `json:"addr,omitempty"`
	Source string `json:"source"` // "lan" — анонс в сети, "incoming" — пытался подключиться
}

type Status struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Folder     string       `json:"folder"`
	Files      int          `json:"files"`
	Bytes      int64        `json:"bytes"`
	LastScan   time.Time    `json:"last_scan,omitzero"`
	Peers      []PeerStatus `json:"peers"`
	Candidates []Candidate  `json:"candidates"`
}

type peerRunner struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	st     PeerStatus
}

func (p *peerRunner) name() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.Name
}

func (p *peerRunner) set(fn func(*PeerStatus)) {
	p.mu.Lock()
	fn(&p.st)
	p.mu.Unlock()
}

type Daemon struct {
	Home string
	ID   *identity.Identity
	Log  *log.Logger

	ctx    context.Context
	folder *folder.Folder
	disc   *discovery.Discovery

	mu       sync.Mutex
	cfg      *config.Config
	peers    map[string]*peerRunner
	incoming map[string]Candidate // отклонённые незнакомые устройства
	learned  map[string]string    // обратные адреса пиров, подключавшихся к нам
	// OnChange вызывается при смене статуса (для трея).
	OnChange func()
}

func New(home string, logger *log.Logger) (*Daemon, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return nil, err
	}
	return &Daemon{Home: home, ID: id, Log: logger, cfg: cfg, peers: map[string]*peerRunner{}, incoming: map[string]Candidate{}, learned: map[string]string{}}, nil
}

func (d *Daemon) Config() config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.cfg
}

func (d *Daemon) changed() {
	if d.OnChange != nil {
		d.OnChange()
	}
}

// Run запускает демон и блокируется до отмены ctx.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	cfg := d.Config()

	ctl, err := listenControl(d.Home)
	if err != nil {
		return err
	}
	defer ctl.Close()

	d.folder, err = folder.Open(cfg.Folder, filepath.Join(d.Home, "index.json"), d.ID.Short(), d.Log)
	if err != nil {
		return err
	}
	d.Log.Printf("устройство %q, ID %s", cfg.Name, identity.Pretty(d.ID.ID))
	d.Log.Printf("папка %s", cfg.Folder)
	if err := d.folder.Scan(); err != nil {
		return fmt.Errorf("скан папки: %w", err)
	}

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:   (&transport.Server{Folder: d.folder, Log: d.Log, OnPeer: d.learn}).Handler(),
		TLSConfig: transport.ServerTLS(d.ID, d.isPeer, d.rejected),
		ErrorLog:  log.New(discardTLSNoise{d.Log}, "", 0),
	}
	go srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	defer srv.Close()
	d.Log.Printf("слушаю TCP :%d", cfg.Port)

	if cfg.Discovery {
		d.disc, err = discovery.Start(ctx, cfg.DiscoveryPort, discovery.Announcement{ID: d.ID.ID, Name: cfg.Name, Port: cfg.Port}, d.Log)
		if err != nil {
			d.Log.Printf("обнаружение в LAN отключено: %v", err)
		}
	}

	go d.watchLoop(ctx)
	go (&http.Server{Handler: d.controlHandler()}).Serve(ctl)

	d.syncPeers()
	<-ctx.Done()
	return nil
}

// watchLoop пересканирует папку по событиям ФС (с задержкой, чтобы пачка
// изменений дала один скан) и раз в минуту на всякий случай.
func (d *Daemon) watchLoop(ctx context.Context) {
	trigger := make(chan struct{}, 1)
	if err := d.folder.Watch(ctx, func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}); err != nil {
		d.Log.Printf("вотчер не запущен, только периодический скан: %v", err)
	}
	tick := time.NewTicker(rescanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			time.Sleep(time.Second)
			select { // события, пришедшие за эту секунду, покрывает тот же скан
			case <-trigger:
			default:
			}
		case <-tick.C:
		}
		if err := d.folder.Scan(); err != nil {
			d.Log.Printf("скан: %v", err)
		}
		d.changed()
	}
}

func (d *Daemon) isPeer(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.FindPeer(id) != nil
}

func (d *Daemon) learn(id, addr string) {
	d.mu.Lock()
	d.learned[id] = addr
	d.mu.Unlock()
}

func (d *Daemon) rejected(id string) {
	d.mu.Lock()
	_, seen := d.incoming[id]
	d.incoming[id] = Candidate{ID: id, Source: "incoming"}
	d.mu.Unlock()
	if !seen {
		d.Log.Printf("отклонено подключение несопряжённого устройства %s", identity.Pretty(id))
		d.changed()
	}
}

// syncPeers приводит набор запущенных клиентов в соответствие с конфигом.
func (d *Daemon) syncPeers() {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]config.Peer{}
	for _, p := range d.cfg.Peers {
		want[p.ID] = p
	}
	for id, pr := range d.peers {
		if _, ok := want[id]; !ok {
			pr.cancel()
			delete(d.peers, id)
		}
	}
	for id, p := range want {
		if pr, ok := d.peers[id]; ok {
			pr.set(func(s *PeerStatus) { s.Name = p.Name })
			continue
		}
		ctx, cancel := context.WithCancel(d.ctx)
		pr := &peerRunner{cancel: cancel, st: PeerStatus{ID: id, Name: p.Name, State: "поиск"}}
		d.peers[id] = pr
		delete(d.incoming, id)
		go d.runPeer(ctx, pr)
	}
}

// addrs — адреса, по которым стоит искать пира: свежий из анонса, затем
// тот, с которого пир сам подключался к нам, затем прописанные вручную.
func (d *Daemon) addrs(id string) []string {
	var out []string
	if d.disc != nil {
		if a, ok := d.disc.Lookup(id); ok {
			out = append(out, a)
		}
	}
	d.mu.Lock()
	if a, ok := d.learned[id]; ok {
		out = append(out, a)
	}
	if p := d.cfg.FindPeer(id); p != nil {
		for _, a := range p.Addrs {
			if _, _, err := net.SplitHostPort(a); err != nil {
				a = net.JoinHostPort(a, strconv.Itoa(config.DefaultPort))
			}
			out = append(out, a)
		}
	}
	d.mu.Unlock()
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (d *Daemon) runPeer(ctx context.Context, pr *peerRunner) {
	id := pr.st.ID
	client := transport.NewClient(d.ID, id, d.Config().Port)
	defer client.Close()
	short := identity.ShortID(id)

	var since uint64
	addr := ""
	attempt := 0
	for ctx.Err() == nil {
		if addr == "" {
			cands := d.addrs(id)
			if len(cands) == 0 {
				pr.set(func(s *PeerStatus) { s.Connected, s.Addr, s.State = false, "", "не найден в сети" })
				d.changed()
				sleepCtx(ctx, 3*time.Second)
				continue
			}
			addr = cands[attempt%len(cands)]
			attempt++
		}

		idx, err := client.Index(ctx, addr, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			pr.mu.Lock()
			wasConnected := pr.st.Connected
			pr.st.Connected, pr.st.State = false, "нет связи"
			pr.mu.Unlock()
			if wasConnected {
				d.Log.Printf("%s: соединение потеряно: %v", pr.st.Name, err)
			}
			d.changed()
			addr, since = "", 0
			sleepCtx(ctx, 3*time.Second)
			continue
		}

		pr.mu.Lock()
		if !pr.st.Connected {
			d.Log.Printf("%s: подключено к %s", pr.st.Name, addr)
		}
		pr.st.Connected, pr.st.Addr = true, addr
		pr.mu.Unlock()

		if idx.Seq != since {
			pr.set(func(s *PeerStatus) { s.State = "синхронизация" })
			d.changed()
			applied, failed := d.folder.ApplyRemote(ctx, short, idx.Files, client.Fetcher(addr))
			if applied > 0 {
				d.Log.Printf("%s: применено изменений: %d", pr.name(), applied)
			}
			if failed > 0 {
				// Повторим полную сверку чуть позже: локальный скан или пир
				// к этому времени, скорее всего, догонят.
				pr.set(func(s *PeerStatus) { s.State = fmt.Sprintf("не удалось: %d, повтор", failed) })
				d.changed()
				since = 0
				sleepCtx(ctx, 5*time.Second)
				continue
			}
			since = idx.Seq
		}
		pr.set(func(s *PeerStatus) { s.State, s.LastSync = "синхронизировано", time.Now() })
		d.changed()
	}
}

func (d *Daemon) Status() Status {
	cfg := d.Config()
	st := Status{ID: d.ID.ID, Name: cfg.Name, Folder: cfg.Folder, Peers: []PeerStatus{}, Candidates: []Candidate{}}
	if d.folder != nil {
		fs := d.folder.Stats()
		st.Files, st.Bytes, st.LastScan = fs.Files, fs.Bytes, fs.LastScan
	}
	d.mu.Lock()
	for _, pr := range d.peers {
		pr.mu.Lock()
		st.Peers = append(st.Peers, pr.st)
		pr.mu.Unlock()
	}
	seen := map[string]bool{}
	if d.disc != nil {
		for _, dev := range d.disc.Devices() {
			if d.cfg.FindPeer(dev.ID) == nil {
				st.Candidates = append(st.Candidates, Candidate{ID: dev.ID, Name: dev.Name, Addr: dev.Addr, Source: "lan"})
				seen[dev.ID] = true
			}
		}
	}
	for id, c := range d.incoming {
		if !seen[id] && d.cfg.FindPeer(id) == nil {
			st.Candidates = append(st.Candidates, c)
		}
	}
	d.mu.Unlock()
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].Name < st.Peers[j].Name })
	return st
}

// Pair добавляет устройство в доверенные. Имя и адрес можно не указывать:
// они подставятся из обнаружения.
func (d *Daemon) Pair(id, name, addr string) error {
	id, err := identity.Normalize(id)
	if err != nil {
		return err
	}
	if id == d.ID.ID {
		return errors.New("это ID этого же устройства")
	}
	if name == "" && d.disc != nil {
		for _, dev := range d.disc.Devices() {
			if dev.ID == id {
				name = dev.Name
			}
		}
	}
	d.mu.Lock()
	err = pairInConfig(d.cfg, id, name, addr)
	if err == nil {
		err = d.cfg.Save(d.Home)
	}
	d.mu.Unlock()
	if err != nil {
		return err
	}
	d.Log.Printf("сопряжено устройство %s", identity.Pretty(id))
	d.syncPeers()
	d.changed()
	return nil
}

func (d *Daemon) Unpair(id string) error {
	id, err := identity.Normalize(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	ok := d.cfg.RemovePeer(id)
	if ok {
		err = d.cfg.Save(d.Home)
	}
	d.mu.Unlock()
	if !ok {
		return errors.New("такого устройства нет среди сопряжённых")
	}
	if err != nil {
		return err
	}
	d.syncPeers()
	d.changed()
	return nil
}

// Reload перечитывает список пиров из конфига (папка и порт — только рестартом).
func (d *Daemon) Reload() error {
	cfg, err := config.Load(d.Home)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.Peers = cfg.Peers
	d.mu.Unlock()
	d.syncPeers()
	d.changed()
	return nil
}

// pairInConfig добавляет или обновляет пира в cfg.
func pairInConfig(cfg *config.Config, id, name, addr string) error {
	if p := cfg.FindPeer(id); p != nil {
		if name != "" {
			p.Name = name
		}
		if addr != "" && !contains(p.Addrs, addr) {
			p.Addrs = append(p.Addrs, addr)
		}
		return nil
	}
	if name == "" {
		name = identity.ShortID(id)
	}
	p := config.Peer{ID: id, Name: name}
	if addr != "" {
		p.Addrs = []string{addr}
	}
	cfg.Peers = append(cfg.Peers, p)
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// discardTLSNoise глушит ожидаемые ошибки рукопожатия от несопряжённых
// устройств и сканеров портов — о них и так сообщает rejected.
type discardTLSNoise struct{ l *log.Logger }

func (w discardTLSNoise) Write(p []byte) (int, error) {
	if os.Getenv("SHAREDIR_DEBUG") != "" {
		w.l.Print(string(p))
	}
	return len(p), nil
}
