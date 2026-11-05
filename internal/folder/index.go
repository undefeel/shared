// Package folder отслеживает состояние синхронизируемой папки.
//
// Каждый файл описывается записью FileInfo с вектором версий: при локальном
// изменении устройство увеличивает свой счётчик. Сравнение векторов говорит,
// чья версия новее, а если изменения параллельные — это конфликт.
package folder

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"time"
)

type Vector map[string]uint64

type Ordering int

const (
	Equal Ordering = iota
	Greater
	Lesser
	Concurrent
)

// Compare сравнивает a с b.
func (a Vector) Compare(b Vector) Ordering {
	var gt, lt bool
	for k, av := range a {
		if bv := b[k]; av > bv {
			gt = true
		} else if av < bv {
			lt = true
		}
	}
	for k, bv := range b {
		if _, ok := a[k]; !ok && bv > 0 {
			lt = true
		}
	}
	switch {
	case gt && lt:
		return Concurrent
	case gt:
		return Greater
	case lt:
		return Lesser
	}
	return Equal
}

func (a Vector) Merge(b Vector) Vector {
	out := make(Vector, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

func (a Vector) Copy() Vector { return a.Merge(nil) }

type FileInfo struct {
	Name    string `json:"name"` // путь относительно корня, через "/", в NFC
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // unix nano
	Exec    bool   `json:"exec,omitempty"`
	Hash    string `json:"hash,omitempty"` // sha256, пусто у удалённых
	Deleted bool   `json:"deleted,omitempty"`
	Version Vector `json:"version"`
	// Disk — фактическое имя на диске, если оно отличается от Name
	// (например, NFD-имя на Linux). Только локальное, пиру не нужно.
	Disk string `json:"disk,omitempty"`
	// StoreDeletedAt is populated only in private group-intent copies, never
	// published or accepted from peers. It preserves the exact store digest.
	StoreDeletedAt int64 `json:"store_deleted_at,omitempty"`
}

func (fi *FileInfo) diskPath() string {
	if fi.Disk != "" {
		return filepath.FromSlash(fi.Disk)
	}
	return filepath.FromSlash(fi.Name)
}

type Stats struct {
	Files    int
	Bytes    int64
	LastScan time.Time
	Kinds    []Kind // по убыванию объёма
}

// Kind — объём файлов одного типа. Label — код (video, images, docs, audio,
// archive, code, other); подпись на нужном языке рисует UI.
type Kind struct {
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
}

var kindByExt = map[string]string{}

func init() {
	for label, exts := range map[string]string{
		"video":   "mp4 mov mkv avi webm m4v wmv flv",
		"images":  "jpg jpeg png gif heic heif webp svg bmp tiff tif raw cr2 nef arw psd",
		"docs":    "pdf doc docx odt rtf txt md xls xlsx ods csv ppt pptx odp pages numbers key epub",
		"audio":   "mp3 flac wav m4a aac ogg opus aiff",
		"archive": "zip tar gz tgz bz2 xz 7z rar dmg iso zst",
		"code":    "go py js ts tsx jsx rs c h cpp swift kt java rb sh json yaml yml toml html css sql",
	} {
		for _, e := range strings.Fields(exts) {
			kindByExt["."+e] = label
		}
	}
}

func kindOf(name string) string {
	if k, ok := kindByExt[strings.ToLower(path.Ext(name))]; ok {
		return k
	}
	return "other"
}

type Folder struct {
	scanError   *ScanError // mu
	scanLogTime time.Time  // mu

	root                *rootFS
	binding             rootBinding // mu
	blocked             *RootError  // mu
	confirmedGeneration uint64      // mu; invalidates work across explicit confirmation

	Root      string
	statePath string
	me        string // короткий ID этого устройства
	log       *log.Logger

	// scanMu сериализует сканирование и запись файлов с пира, чтобы скан
	// не принял только что записанный файл за удалённый (и наоборот).
	scanMu           sync.Mutex
	applyMu          sync.Mutex
	applyCoordinator *groupCoordinator
	downloadMu       sync.Mutex
	downloadPool     *downloadPool
	// Lock order: scanMu → saveMu → mu. I/O never holds mu.
	saveMu            sync.Mutex
	generation        uint64 // saveMu; attempts, including failed saves
	durableGeneration uint64 // saveMu; advances only after directory flush
	store             *indexStore
	persistHooks      persistHooks // set before concurrent use, for fault injection
	waitEntered       func()       // Test barrier, set before concurrent use; nil in production.

	mu        sync.Mutex
	files     map[string]*FileInfo
	clock     uint64
	seq       uint64
	changed   chan struct{}
	lastScan  time.Time
	ignore    []string
	hashCache map[string]scanHashCache
	stability *stabilityTracker
}

type persistHooks struct {
	probe    PersistenceProbe
	counters PersistenceCounters
	marshal  func(any) ([]byte, error)
	rename   func(string, string) error
	syncFile func(*os.File) error
	syncDir  func(*os.File) error
}

type persisted struct {
	Schema              int         `json:"schema"`
	Binding             rootBinding `json:"binding"`
	Blocked             *RootError  `json:"blocked,omitempty"`
	ConfirmedGeneration uint64      `json:"confirmed_generation,omitempty"`
	Generation          uint64      `json:"generation,omitempty"`
	Clock               uint64      `json:"clock"`
	Files               []*FileInfo `json:"files"`
}

func Open(root, statePath, me string, logger *log.Logger) (*Folder, error) {
	f := &Folder{statePath: statePath}
	if err := f.completeReset(); err != nil {
		return nil, err
	}
	var legacy *Folder
	if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "index.snapshot")); os.IsNotExist(err) {
		// Bootstrap still uses the legacy root policy, but never bypass validation.
		if _, e := os.Stat(statePath); os.IsNotExist(e) {
			legacy, e = openLegacy(root, statePath, me, logger)
			if e != nil {
				return nil, e
			}
		}
	}
	store, err := migrateStore(statePath, func() error {
		if legacy != nil {
			return nil
		}
		var e error
		legacy, e = openLegacy(root, statePath, me, logger)
		return e
	}, func(file *os.File) error { return f.persistHooks.durableFile(file, "migration") },
		func(dir *os.File) error { return f.persistHooks.fsyncOnly(dir, "migration") }, &f.persistHooks)
	if legacy != nil {
		legacy.Close()
	}
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		store.close()
		return nil, err
	}
	f.Root, f.me, f.log, f.store = root, me, logger, store
	f.files, f.hashCache = map[string]*FileInfo{}, map[string]scanHashCache{}
	f.seq, f.changed = uint64(time.Now().UnixNano()), make(chan struct{})
	p := storePersisted(store.state)
	f.binding, f.blocked, f.confirmedGeneration = p.Binding, p.Blocked, p.ConfirmedGeneration
	f.clock, f.generation, f.durableGeneration = p.Clock, p.Generation, p.Generation
	for _, fi := range p.Files {
		f.files[fi.Name] = fi
	}
	f.wireStore()
	if err = f.initRoot(nil); err == nil {
		err = f.recoverApplyIntent()
	}
	if err == nil && f.root != nil {
		var rules []string
		rules, err = readIgnore(f.rootScanIO(), f.root)
		if err == nil {
			f.ignore = rules
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func storePersisted(s storeSnapshot) persisted {
	p := persisted{Schema: 2, Generation: s.Generation, Clock: s.Clock, Binding: s.Control.Binding,
		Blocked: s.Control.Blocked, ConfirmedGeneration: s.Control.ConfirmedGeneration}
	for _, v := range s.Files {
		fi := &FileInfo{Name: v.Name, Disk: v.Disk, Hash: v.Hash, Size: v.Size, ModTime: v.ModTime, Exec: v.Exec, Deleted: v.Deleted, Version: Vector{}}
		for _, pair := range v.Version {
			fi.Version[pair.Peer] = pair.Counter
		}
		p.Files = append(p.Files, fi)
	}
	return p
}

func (f *Folder) wireStore() {
	f.store.write = func(b []byte) (int, error) { return f.persistHooks.write("log", f.store.log.Name(), f.store.log, b) }
	f.store.flush = func(file *os.File) error {
		if f.persistHooks.syncFile != nil {
			return f.persistHooks.syncFile(file)
		}
		return f.persistHooks.durableFile(file, "G4")
	}
}

func openLegacy(root, statePath, me string, logger *log.Logger) (*Folder, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	f := &Folder{
		Root:      root,
		statePath: statePath,
		me:        me,
		log:       logger,
		files:     map[string]*FileInfo{},
		hashCache: map[string]scanHashCache{},
		// seq стартует со времени запуска: клиент, помнящий seq прошлого
		// процесса, сразу получит свежий индекс.
		seq:     uint64(time.Now().UnixNano()),
		changed: make(chan struct{}),
	}
	data, err := os.ReadFile(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		var p persisted
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		if p.Schema != 0 && p.Schema != 2 {
			return nil, errors.New("unsupported index schema")
		}
		f.binding, f.blocked, f.confirmedGeneration = p.Binding, p.Blocked, p.ConfirmedGeneration
		f.clock = p.Clock
		f.generation, f.durableGeneration = p.Generation, p.Generation
		seen := make(map[string]bool, len(p.Files))
		for _, fi := range p.Files {
			if fi == nil || !validDisk(fi.Name, fi.Disk) {
				return nil, errors.New("invalid file name or disk path in index")
			}
			if seen[fi.Name] {
				return nil, errors.New("duplicate file name in index: " + fi.Name)
			}
			seen[fi.Name] = true
			f.files[fi.Name] = fi
		}
	}
	if err := f.initRoot(data); err != nil {
		return nil, err
	}
	err = f.recoverIntent()
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// tick выдаёт следующее значение логических часов устройства. Вызывать под mu.
func (f *Folder) tick(v Vector) uint64 {
	for _, n := range v {
		f.clock = max(f.clock, n)
	}
	f.clock++
	return f.clock
}

// notify отмечает изменение индекса и будит ожидающих. Вызывать под mu.
func (f *Folder) notify() {
	f.seq++
	close(f.changed)
	f.changed = make(chan struct{})
}

// save serializes writers before taking a deep snapshot, including mergeOnly
// changes made under mu. A delayed writer cannot overwrite a newer snapshot.
func (f *Folder) save() error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.saveLocked()
}

// saveLocked requires saveMu.
func (f *Folder) saveLocked() error {
	f.generation++
	f.mu.Lock()
	p := persisted{Generation: f.generation, Clock: f.clock, Files: make([]*FileInfo, 0, len(f.files))}
	for _, fi := range f.files {
		c := *fi
		c.Version = fi.Version.Copy()
		p.Files = append(p.Files, &c)
	}
	f.mu.Unlock()
	return f.persistLocked(p)
}

// persistLocked appends one durable delta (JSON only while migrating); caller holds saveMu.
func (f *Folder) persistLocked(p persisted) error {
	f.mu.Lock()
	p.Schema, p.Binding, p.ConfirmedGeneration = 2, f.binding, f.confirmedGeneration
	if f.blocked != nil {
		c := *f.blocked
		p.Blocked = &c
	}
	f.mu.Unlock()
	if f.store != nil {
		return f.persistStore(p)
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Name < p.Files[j].Name })
	marshal := f.persistHooks.marshal
	if marshal == nil {
		marshal = json.Marshal
	}
	data, err := marshal(p)
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(f.statePath))
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp, err := os.CreateTemp(filepath.Dir(f.statePath), ".sharedir-index-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if n, err := tmp.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	flush := f.persistHooks.syncFile
	if flush == nil {
		flush = syncFile
	}
	if err := flush(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	rename := f.persistHooks.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tmp.Name(), f.statePath); err != nil {
		return err
	}
	flush = f.persistHooks.syncDir
	if flush == nil {
		flush = syncDir
	}
	if err := flush(dir); err != nil {
		return err
	}
	f.durableGeneration = p.Generation
	return nil
}

// persistStore preserves local store-only metadata and unrelated control history.
func (f *Folder) persistStore(p persisted) error {
	s := f.store
	if f.persistHooks.marshal != nil {
		if _, err := f.persistHooks.marshal(p); err != nil {
			return err
		}
	}
	c := s.state.Control
	c.Binding, c.Blocked, c.ConfirmedGeneration = p.Binding, p.Blocked, p.ConfirmedGeneration
	frame := storeFrame{Kind: "Control", PreviousGeneration: s.state.Generation, Generation: s.state.Generation + 1, Clock: p.Clock, Control: &c}
	old := make(map[string]storeFile, len(s.state.Files))
	for _, v := range s.state.Files {
		old[v.Name] = v
	}
	for _, fi := range p.Files {
		v := *recoveryFile(fi)
		before, exists := old[v.Name]
		if exists {
			v.DeletedAt = before.DeletedAt
		}
		if !v.Deleted {
			v.DeletedAt = 0
		} else if v.DeletedAt == 0 {
			v.DeletedAt = f.persistHooks.clockNow().UnixNano()
		}
		if !exists || !reflect.DeepEqual(v, before) {
			op := storeOperation{Name: v.Name, After: &v}
			if exists {
				op.Before = storeDigest(before)
			}
			frame.Operations = append(frame.Operations, op)
		}
		delete(old, v.Name)
	}
	for name, v := range old {
		frame.Operations = append(frame.Operations, storeOperation{Name: name, Before: storeDigest(v), DeleteRecord: true})
	}
	if len(frame.Operations) == 0 && frame.Clock == s.state.Clock && reflect.DeepEqual(c, s.state.Control) {
		f.generation = s.state.Generation
		return nil
	}
	sort.Slice(frame.Operations, func(i, j int) bool { return frame.Operations[i].Name < frame.Operations[j].Name })
	if len(frame.Operations) != 0 {
		frame.Kind = "Commit"
	}
	if _, err := rand.Read(frame.TxID[:]); err != nil {
		return err
	}
	if err := s.append(frame); err != nil {
		return err
	}
	f.generation, f.durableGeneration = frame.Generation, frame.Generation
	return nil
}

// Snapshot возвращает копию индекса и его текущий seq.
func (f *Folder) Snapshot() ([]FileInfo, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FileInfo, 0, len(f.files))
	for _, fi := range f.files {
		if ignoredWith(f.ignore, fi.Name) {
			continue
		}
		c := *fi
		c.Disk = ""
		c.Version = fi.Version.Copy()
		out = append(out, c)
	}
	return out, f.seq
}

// Kick будит всех, кто ждёт в Wait, не меняя индекс.
func (f *Folder) Kick() {
	f.mu.Lock()
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// Wait блокируется, пока seq не станет отличным от since, или до таймаута.
func (f *Folder) Wait(since uint64, timeout time.Duration) {
	f.mu.Lock()
	if f.seq != since {
		f.mu.Unlock()
		return
	}
	ch := f.changed
	f.mu.Unlock()
	if f.waitEntered != nil {
		f.waitEntered()
	}
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

// Lookup возвращает копию записи и абсолютный путь файла на диске.
func (f *Folder) Lookup(name string) (FileInfo, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.files[name]
	if !ok {
		return FileInfo{}, "", false
	}
	c := *fi
	c.Version = fi.Version.Copy()
	return c, filepath.Join(f.Root, fi.diskPath()), true
}

func (f *Folder) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Stats{LastScan: f.lastScan}
	kinds := map[string]int64{}
	for _, fi := range f.files {
		if !fi.Deleted {
			s.Files++
			s.Bytes += fi.Size
			kinds[kindOf(fi.Name)] += fi.Size
		}
	}
	for label, n := range kinds {
		s.Kinds = append(s.Kinds, Kind{label, n})
	}
	sort.Slice(s.Kinds, func(i, j int) bool { return s.Kinds[i].Bytes > s.Kinds[j].Bytes })
	return s
}

// PersistenceProbe is installed before concurrent Folder use. Callbacks must be
// concurrency-safe and must not reenter Folder. Nil callbacks leave I/O intact.
// Flush observes each attempted syscall separately; Fault can fail that attempt.
// Write observes actual returned bytes, including short writes and failures.
type PersistenceProbe struct {
	Write    func(kind, path string, n int, err error)
	Flush    func(kind, phase string, dev, inode uint64)
	Fault    func(kind, phase string, dev, inode uint64) error
	Phase    func(txID, phase string)
	ClockNow func() time.Time
}

// PersistenceCounters counts syscall attempts and actual returned write bytes.
// Linux has no FULLFSYNC attempt; counters never count a synthetic flush there.
type PersistenceCounters struct{ Bytes, Fsync, Fullfsync, Barrierfsync uint64 }

func (f *Folder) SetPersistenceProbe(p PersistenceProbe) { f.persistHooks.probe = p }
func (f *Folder) PersistenceStats() PersistenceCounters {
	c := &f.persistHooks.counters
	return PersistenceCounters{atomic.LoadUint64(&c.Bytes), atomic.LoadUint64(&c.Fsync), atomic.LoadUint64(&c.Fullfsync), atomic.LoadUint64(&c.Barrierfsync)}
}

func (h *persistHooks) write(kind, path string, w io.Writer, data []byte) (int, error) {
	n, err := w.Write(data)
	if n > 0 {
		atomic.AddUint64(&h.counters.Bytes, uint64(n))
	}
	if h.probe.Write != nil {
		h.probe.Write(kind, path, n, err)
	}
	return n, err
}
func (h *persistHooks) phase(txID, phase string) {
	if h.probe.Phase != nil {
		h.probe.Phase(txID, phase)
	}
}
func (h *persistHooks) clockNow() time.Time {
	if h.probe.ClockNow != nil {
		return h.probe.ClockNow()
	}
	return time.Now()
}
func fileDevice(file *os.File) (uint64, uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}
func (h *persistHooks) flush(file *os.File, phase, kind string, call func() error) error {
	dev, inode, err := fileDevice(file)
	if err != nil {
		return err
	}
	switch kind {
	case "fsync":
		atomic.AddUint64(&h.counters.Fsync, 1)
	case "FULLFSYNC":
		atomic.AddUint64(&h.counters.Fullfsync, 1)
	case "BARRIERFSYNC":
		atomic.AddUint64(&h.counters.Barrierfsync, 1)
	}
	if h.probe.Flush != nil {
		h.probe.Flush(kind, phase, dev, inode)
	}
	if h.probe.Fault != nil {
		if err := h.probe.Fault(kind, phase, dev, inode); err != nil {
			return err
		}
	}
	return call()
}

// fsyncOnly is used for every prepared inode and directory before device flush.
func (h *persistHooks) fsyncOnly(file *os.File, phase string) error {
	return h.flush(file, phase, "fsync", func() error { return platformOps().fsync(int(file.Fd())) })
}

// durableFile preserves fsync then hardware flush for intent/commit/snapshot.
func (h *persistHooks) durableFile(file *os.File, phase string) error {
	if err := h.fsyncOnly(file, phase); err != nil {
		return err
	}
	return h.fullSyncDevices([]*os.File{file}, phase)
}
func (h *persistHooks) fullSyncDevices(files []*os.File, phase string) error {
	return h.fullSyncDevicesWithOps(files, phase, platformOps(), fileDevice)
}

// Deduplication is local to one phase invocation. Representatives stay open
// throughout the call; state and root devices must both be supplied by caller.
func (h *persistHooks) fullSyncDevicesWithOps(files []*os.File, phase string, ops platformFSOps, identity func(*os.File) (uint64, uint64, error)) error {
	if ops.fullSync == nil {
		return nil
	}
	seen := make(map[uint64]bool)
	for _, file := range files {
		dev, _, err := identity(file)
		if err != nil {
			return err
		}
		if seen[dev] {
			continue
		}
		if err := h.flush(file, phase, "FULLFSYNC", func() error { return ops.syncDevice(file) }); err != nil {
			return err
		}
		seen[dev] = true
	}
	return nil
}
