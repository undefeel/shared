// Package app собирает демон: папку, сервер, клиентов пиров, обнаружение и
// управляющий сокет.
package app

import (
	"context"
	"crypto/tls"
	"errors"
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

// Состояния пира. Это коды для UI: текст на нужном языке подставляет клиент.
const (
	StateSearching  = "searching"   // ищем адрес
	StateNotFound   = "not_found"   // ни одного адреса: нет в LAN и не прописан
	StateOffline    = "offline"     // адрес есть, но пир не отвечает
	StateSyncing    = "syncing"     // применяем изменения пира
	StateSynced     = "synced"      // всё сошлось
	StateRetry      = "retry"       // часть файлов не применилась, скоро повтор
	StatePaused     = "paused"      // пауза у нас
	StatePeerPaused = "peer_paused" // пауза у пира
)

type PeerStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Syncing   bool      `json:"syncing"`
	Addr      string    `json:"addr,omitempty"`
	State     string    `json:"state"`
	Failed    int       `json:"failed,omitempty"` // для StateRetry
	LastSync  time.Time `json:"last_sync,omitzero"`
	Progress  *Progress `json:"progress,omitempty"`
}

// Progress — ход текущего приёма изменений от пира.
type Progress struct {
	Files     int   `json:"files"`
	DoneFiles int   `json:"done_files"`
	Bytes     int64 `json:"bytes"`
	DoneBytes int64 `json:"done_bytes"`
}

type Candidate struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Addr   string `json:"addr,omitempty"`
	Source string `json:"source"` // "lan" — анонс в сети, "incoming" — пытался подключиться
}

type Status struct {
	ScanError   *folder.ScanError `json:"scan_error,omitempty"`
	FolderError *folder.RootError `json:"folder_error,omitempty"`
	Running     bool              `json:"running"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Folder      string            `json:"folder"`
	Files       int               `json:"files"`
	Bytes       int64             `json:"bytes"`
	LastScan    time.Time         `json:"last_scan,omitzero"`
	Paused      bool              `json:"paused"`
	Syncing     bool              `json:"syncing"`
	Relay       *RelayStatus      `json:"relay,omitempty"`
	Peers       []PeerStatus      `json:"peers"`
	Candidates  []Candidate       `json:"candidates"`
	Kinds       []folder.Kind     `json:"kinds"`
	Days        []Day             `json:"days"`   // последние 7 дней, старые первыми
	Recent      []Event           `json:"recent"` // новые первыми
}

type peerRunner struct {
	cancel context.CancelFunc // nil, пока синхронизация на паузе
	lan    chan struct{}      // пир появился в LAN: пора уходить с relay
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
	act    *activity

	lifecycle    sync.Mutex
	workers      sync.WaitGroup
	watchCancel  context.CancelFunc
	watchDone    chan struct{}
	mu           sync.Mutex
	cfg          *config.Config
	peers        map[string]*peerRunner
	connections  transport.Connections
	incomingSeen map[string]time.Time
	incoming     map[string]Candidate // отклонённые незнакомые устройства
	learned      map[string]string    // обратные адреса пиров, подключавшихся к нам
	relaySt      *RelayStatus
	relayKick    chan struct{} // смена адреса relay в конфиге
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
	return &Daemon{Home: home, ID: id, Log: logger, cfg: cfg, act: loadActivity(filepath.Join(home, "activity.json")), peers: map[string]*peerRunner{}, incoming: map[string]Candidate{}, learned: map[string]string{}, relayKick: make(chan struct{}, 1)}, nil
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
	defer func() { d.lifecycle.Lock(); defer d.lifecycle.Unlock(); d.stopFolder(); _ = d.folder.Close() }()
	d.Log.Printf("устройство %q, ID %s", cfg.Name, identity.Pretty(d.ID.ID))
	d.Log.Printf("папка %s", cfg.Folder)
	if err := d.folder.Scan(); err != nil {
		d.Log.Printf("скан папки: %v", err)
	}

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           (&transport.Server{Folder: d.folder, Log: d.Log, OnPeer: d.learn, Paused: d.Paused, OnServed: d.served, Allowed: d.isPeer}).Handler(),
		TLSConfig:         transport.ServerTLS(d.ID, d.isPeer, d.rejected),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		ConnState: func(c net.Conn, state http.ConnState) {
			d.connections.State(c, state)
			if state == http.StateActive || state == http.StateIdle {
				d.connections.Revoke(d.isPeer)
			}
		},
		ErrorLog: log.New(discardTLSNoise{d.Log}, "", 0),
	}
	go srv.Serve(tls.NewListener(d.connections.Limit(ln), srv.TLSConfig))
	// Тот же сервер принимает и каналы, пришедшие через relay.
	relayLn := newConnListener()
	go srv.Serve(tls.NewListener(d.connections.Limit(relayLn), srv.TLSConfig))
	go d.relayLoop(ctx, relayLn)
	defer srv.Close()
	d.Log.Printf("слушаю TCP :%d", cfg.Port)

	if cfg.Discovery {
		d.disc, err = discovery.StartWithKeep(ctx, cfg.DiscoveryPort, discovery.Announcement{ID: d.ID.ID, Name: cfg.Name, Port: cfg.Port}, d.Log, d.peerOnLAN, d.isPeer)
		if err != nil {
			d.Log.Printf("обнаружение в LAN отключено: %v", err)
		}
	}

	d.startWatch()
	go (&http.Server{Handler: d.controlHandler()}).Serve(ctl)

	d.syncPeers()
	<-ctx.Done()
	return nil
}

// watchLoop пересканирует папку по событиям ФС (с задержкой, чтобы пачка
// изменений дала один скан) и раз в минуту на всякий случай.
func (d *Daemon) watchLoop(ctx context.Context) {
	generation := d.folder.ConfirmedGeneration()
	trigger := make(chan struct{}, 1)
	watchStop, watcherDone, err := d.folder.Watch(ctx, func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	})
	if err != nil {
		d.Log.Printf("вотчер не запущен, только периодический скан: %v", err)
	} else {
		defer func() {
			watchStop()
			<-watcherDone
		}()
	}
	tick := time.NewTicker(rescanInterval)
	defer tick.Stop()
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	defer d.act.flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			d.act.flush()
			continue
		case <-trigger:
			time.Sleep(time.Second)
			select { // события, пришедшие за эту секунду, покрывает тот же скан
			case <-trigger:
			default:
			}
		case <-tick.C:
		}
		if ctx.Err() != nil {
			return
		}
		if err := d.folder.ScanGeneration(ctx, generation); err != nil {
			if d.folder.Blocked() != nil {
				d.mu.Lock()
				for _, p := range d.peers {
					if p.cancel != nil {
						p.cancel()
					}
				}
				d.mu.Unlock()
			}
		}
		d.changed()
	}
}

func (d *Daemon) isPeer(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.FindPeer(id) != nil
}

func (d *Daemon) peerName(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p := d.cfg.FindPeer(id); p != nil {
		return p.Name
	}
	return identity.ShortID(id)
}

func (d *Daemon) served(id string, fi folder.FileInfo) {
	d.act.add(Event{Time: time.Now(), Kind: "out", Name: fi.Name, Peer: d.peerName(id), Size: fi.Size})
	d.changed()
}

func (d *Daemon) Paused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.Paused
}

// SetPaused ставит синхронизацию на паузу или снимает с неё. Пока пауза,
// мы не тянем изменения и не отдаём их пирам; локальный скан продолжается,
// так что после возобновления всё накопленное уйдёт.
func (d *Daemon) SetPaused(paused bool) error {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	d.mu.Lock()
	d.cfg.Paused = paused
	err := d.cfg.Save(d.Home)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if paused {
		d.stopFolder()
		d.startWatch()
		d.folder.Kick() // висящие long-poll пиров сразу получат «пауза»
		d.Log.Printf("синхронизация на паузе")
	} else {
		d.Log.Printf("синхронизация возобновлена")
	}
	d.syncPeersLocked()
	d.changed()
	return nil
}

func (d *Daemon) learn(id, addr string) {
	d.mu.Lock()
	d.learned[id] = addr
	d.mu.Unlock()
}

func (d *Daemon) rejected(id string) {
	d.mu.Lock()
	d.pruneIncoming(time.Now())
	if d.incomingSeen == nil {
		d.incomingSeen = make(map[string]time.Time)
	}
	_, seen := d.incoming[id]
	if !seen && len(d.incoming) >= 256 {
		d.mu.Unlock()
		return
	}
	d.incomingSeen[id] = time.Now()
	d.incoming[id] = Candidate{ID: id, Source: "incoming"}
	d.mu.Unlock()
	if !seen {
		d.Log.Printf("отклонено подключение несопряжённого устройства %s", identity.Pretty(id))
		d.changed()
	}
}

// syncPeers приводит набор запущенных клиентов в соответствие с конфигом.
func (d *Daemon) syncPeers() {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	d.syncPeersLocked()
}

func (d *Daemon) syncPeersLocked() {
	d.connections.Revoke(d.isPeer)
	blocked := d.folder.Blocked() != nil
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]config.Peer{}
	for _, p := range d.cfg.Peers {
		want[p.ID] = p
	}
	for id, pr := range d.peers {
		if _, ok := want[id]; !ok {
			if pr.cancel != nil {
				pr.cancel()
			}
			delete(d.peers, id)
		}
	}
	for id, p := range want {
		pr, ok := d.peers[id]
		if !ok {
			pr = &peerRunner{lan: make(chan struct{}, 1), st: PeerStatus{ID: id, Name: p.Name, State: StateSearching}}
			d.peers[id] = pr
			delete(d.incoming, id)
		}
		pr.set(func(s *PeerStatus) { s.Name = p.Name })
		switch {
		case (d.cfg.Paused || blocked) && pr.cancel != nil:
			pr.cancel()
			pr.cancel = nil
			pr.set(func(s *PeerStatus) { s.Connected, s.Syncing, s.Progress, s.State = false, false, nil, StatePaused })
		case d.cfg.Paused || blocked:
			pr.set(func(s *PeerStatus) { s.State = StatePaused })
		case pr.cancel == nil:
			ctx, cancel := context.WithCancel(d.ctx)
			pr.cancel = cancel
			pr.set(func(s *PeerStatus) { s.State = StateSearching })
			d.workers.Add(1)
			go func() { defer d.workers.Done(); d.runPeer(ctx, pr) }()
		}
	}
}

// peerProgress переводит ход ApplyRemote в статус пира и ленту событий.
type peerProgress struct {
	d  *Daemon
	pr *peerRunner
}

func (p peerProgress) Plan(files int, bytes int64) {
	p.pr.set(func(s *PeerStatus) { s.Progress = &Progress{Files: files, Bytes: bytes} })
	p.d.changed()
}

func (p peerProgress) Done(fi folder.FileInfo, ev folder.Event, bytes int64) {
	p.pr.set(func(s *PeerStatus) {
		if s.Progress != nil {
			s.Progress.DoneFiles++
			s.Progress.DoneBytes += bytes
		}
	})
	p.d.act.add(Event{Time: time.Now(), Kind: string(ev), Name: fi.Name, Peer: p.pr.name(), Size: bytes})
	p.d.changed()
}

// addrs — адреса, по которым стоит искать пира: свежий из анонса, затем
// тот, с которого пир сам подключался к нам, затем прописанные вручную,
// и в самом конце — через relay.
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
	if d.cfg.Relay != "" {
		out = append(out, transport.RelayHost) // последним: напрямую быстрее
	}
	d.mu.Unlock()
	return out
}

// peerOnLAN — discovery увидел устройство в сети. Если с ним сейчас висит
// long-poll через relay, runPeer прервёт его и сразу перейдёт на прямую связь,
// не дожидаясь ответа пира (до pollTimeout).
func (d *Daemon) peerOnLAN(dev discovery.Device) {
	d.mu.Lock()
	pr := d.peers[dev.ID]
	d.mu.Unlock()
	if pr == nil {
		return
	}
	select {
	case pr.lan <- struct{}{}:
	default:
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (d *Daemon) runPeer(ctx context.Context, pr *peerRunner) {
	id := pr.st.ID
	client := transport.NewClient(d.ID, id, d.Config().Port, d.relayDial(id))
	defer client.Close()
	short := identity.ShortID(id)

	var since uint64
	addr := ""
	attempt := 0
	for ctx.Err() == nil {
		if addr == "" {
			cands := d.addrs(id)
			if len(cands) == 0 {
				pr.set(func(s *PeerStatus) { s.Connected, s.Addr, s.State = false, "", StateNotFound })
				d.changed()
				sleepCtx(ctx, 3*time.Second)
				continue
			}
			addr = cands[attempt%len(cands)]
			attempt++
		}

		// Через relay long-poll прерываем, как только пир появится в LAN.
		pollCtx, stopPoll := ctx, func() {}
		if addr == transport.RelayHost {
			var cancel context.CancelFunc
			pollCtx, cancel = context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				select {
				case <-pr.lan:
					cancel()
				case <-done:
				}
			}()
			stopPoll = func() { close(done); cancel() }
		}
		idx, err := client.Index(pollCtx, addr, since)
		toLAN := err != nil && pollCtx.Err() != nil && ctx.Err() == nil
		stopPoll()
		if toLAN {
			d.Log.Printf("%s: пир в локальной сети, переключаюсь с relay напрямую", pr.name())
			addr, since, attempt = "", 0, 0
			continue
		}
		if errors.Is(err, transport.ErrPeerPaused) {
			pr.set(func(s *PeerStatus) { s.Connected, s.Addr, s.State = true, addr, StatePeerPaused })
			d.changed()
			since = 0
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			pr.mu.Lock()
			wasConnected := pr.st.Connected
			pr.st.Connected, pr.st.State = false, StateOffline
			pr.mu.Unlock()
			if wasConnected {
				d.Log.Printf("%s: соединение потеряно: %v", pr.name(), err)
			}
			d.changed()
			addr, since = "", 0
			sleepCtx(ctx, 3*time.Second)
			continue
		}

		pr.mu.Lock()
		if !pr.st.Connected || pr.st.Addr != addr {
			d.Log.Printf("%s: подключено к %s", pr.st.Name, addr)
		}
		pr.st.Connected, pr.st.Addr = true, addr
		pr.mu.Unlock()

		if idx.Seq != since {
			pr.set(func(s *PeerStatus) { s.State, s.Syncing = StateSyncing, true })
			d.changed()
			applied, failed := d.folder.ApplyRemote(ctx, short, idx.Files, client.Fetcher(addr), peerProgress{d, pr})
			pr.set(func(s *PeerStatus) { s.Syncing, s.Progress = false, nil })
			if ctx.Err() != nil {
				return // пауза или удаление пира: статус уже выставил syncPeers
			}
			if applied > 0 {
				d.Log.Printf("%s: применено изменений: %d", pr.name(), applied)
			}
			if failed > 0 {
				// Повторим полную сверку чуть позже: локальный скан или пир
				// к этому времени, скорее всего, догонят.
				pr.set(func(s *PeerStatus) { s.State, s.Failed = StateRetry, failed })
				d.changed()
				since = 0
				sleepCtx(ctx, 5*time.Second)
				continue
			}
			since = idx.Seq
		}
		pr.set(func(s *PeerStatus) { s.State, s.LastSync, s.Failed = StateSynced, time.Now(), 0 })
		d.changed()

		// Вернулись домой: пир снова виден в LAN — уходим с relay на прямую связь.
		if addr == transport.RelayHost && d.disc != nil {
			if _, ok := d.disc.Lookup(id); ok {
				d.Log.Printf("%s: пир в локальной сети, переключаюсь с relay напрямую", pr.name())
				addr, since, attempt = "", 0, 0
			}
		}
	}
}

func (d *Daemon) Status() Status {
	cfg := d.Config()
	st := Status{Running: true, ID: d.ID.ID, Name: cfg.Name, Folder: cfg.Folder, Paused: cfg.Paused, Peers: []PeerStatus{}, Candidates: []Candidate{}, Kinds: []folder.Kind{}}
	if d.folder != nil {
		fs := d.folder.Stats()
		st.Files, st.Bytes, st.LastScan = fs.Files, fs.Bytes, fs.LastScan
		st.FolderError = d.folder.Blocked()
		st.ScanError = d.folder.LastScanError()
		if fs.Kinds != nil {
			st.Kinds = fs.Kinds
		}
	}
	st.Days, st.Recent = d.act.snapshot(7, 20)
	d.mu.Lock()
	if d.relaySt != nil {
		rs := *d.relaySt
		st.Relay = &rs
	}
	d.mu.Unlock()
	if st.Recent == nil {
		st.Recent = []Event{}
	}
	d.mu.Lock()
	for _, pr := range d.peers {
		pr.mu.Lock()
		ps := pr.st
		if ps.Progress != nil {
			cp := *ps.Progress
			ps.Progress = &cp
		}
		st.Syncing = st.Syncing || ps.Syncing
		st.Peers = append(st.Peers, ps)
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
	d.pruneIncoming(time.Now())
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
		return errors.New("that is this device's own ID")
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
	d.connections.Revoke(d.isPeer)
	if !ok {
		return errors.New("no such paired device")
	}
	if err != nil {
		return err
	}
	d.syncPeers()
	d.changed()
	return nil
}

// Reload перечитывает конфиг; смена папки требует явного подтверждения.
func (d *Daemon) Reload() error {
	cfg, err := config.Load(d.Home)
	if err != nil {
		return err
	}
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	if cfg.Folder != d.Config().Folder {
		d.stopFolder()
		if err := d.folder.SetConfiguredRoot(cfg.Folder); err != nil && d.folder.Blocked() == nil {
			return err
		}
		d.mu.Lock()
		d.cfg.Folder = cfg.Folder
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.cfg.Peers = cfg.Peers
	relayChanged := d.cfg.Relay != cfg.Relay
	d.cfg.Relay = cfg.Relay
	d.mu.Unlock()
	if relayChanged {
		select {
		case d.relayKick <- struct{}{}:
		default:
		}
	}
	d.syncPeersLocked()
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

// Called with d.mu held; bounded candidates expire after three discovery intervals.
func (d *Daemon) pruneIncoming(now time.Time) {
	for id, t := range d.incomingSeen {
		if now.Sub(t) > 90*time.Second {
			delete(d.incoming, id)
			delete(d.incomingSeen, id)
		}
	}
}

// stopFolder is called with lifecycle held, without mu while draining runners.
func (d *Daemon) stopFolder() {
	d.mu.Lock()
	for _, p := range d.peers {
		if p.cancel != nil {
			p.cancel()
			p.cancel = nil
		}
	}
	d.mu.Unlock()
	d.workers.Wait()
	if d.watchCancel != nil {
		d.watchCancel()
		<-d.watchDone
		d.watchCancel = nil
	}
}
func (d *Daemon) startWatch() {
	ctx, cancel := context.WithCancel(d.ctx)
	d.watchCancel = cancel
	d.watchDone = make(chan struct{})
	go func() { defer close(d.watchDone); d.watchLoop(ctx) }()
}
func (d *Daemon) ConfirmFolder(token, action string) error {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	d.stopFolder()
	err := d.folder.Confirm(token, action, d.Config().Folder)
	d.startWatch()
	d.syncPeersLocked()
	d.changed()
	return err
}
