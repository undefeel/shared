// Package folder отслеживает состояние синхронизируемой папки.
//
// Каждый файл описывается записью FileInfo с вектором версий: при локальном
// изменении устройство увеличивает свой счётчик. Сравнение векторов говорит,
// чья версия новее, а если изменения параллельные — это конфликт.
package folder

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	Root      string
	statePath string
	me        string // короткий ID этого устройства
	log       *log.Logger

	// scanMu сериализует сканирование и запись файлов с пира, чтобы скан
	// не принял только что записанный файл за удалённый (и наоборот).
	scanMu sync.Mutex

	mu       sync.Mutex
	files    map[string]*FileInfo
	clock    uint64
	seq      uint64
	changed  chan struct{}
	lastScan time.Time
	ignore   []string
}

type persisted struct {
	Clock uint64      `json:"clock"`
	Files []*FileInfo `json:"files"`
}

func Open(root, statePath, me string, logger *log.Logger) (*Folder, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	f := &Folder{
		Root:      root,
		statePath: statePath,
		me:        me,
		log:       logger,
		files:     map[string]*FileInfo{},
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
		f.clock = p.Clock
		for _, fi := range p.Files {
			f.files[fi.Name] = fi
		}
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

func (f *Folder) save() error {
	f.mu.Lock()
	p := persisted{Clock: f.clock, Files: make([]*FileInfo, 0, len(f.files))}
	for _, fi := range f.files {
		p.Files = append(p.Files, fi)
	}
	f.mu.Unlock()
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Name < p.Files[j].Name })
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := f.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.statePath)
}

// Snapshot возвращает копию индекса и его текущий seq.
func (f *Folder) Snapshot() ([]FileInfo, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FileInfo, 0, len(f.files))
	for _, fi := range f.files {
		c := *fi
		c.Disk = ""
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
	return *fi, filepath.Join(f.Root, fi.diskPath()), true
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
