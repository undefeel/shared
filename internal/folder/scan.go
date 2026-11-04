package folder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

const (
	tmpPrefix  = ".sharedir-tmp-"
	ignoreFile = ".sharedirignore"
)

// Служебный мусор ОС, который синхронизировать бессмысленно.
var builtinIgnore = []string{
	".DS_Store", "._*", ".Spotlight-V100", ".Trashes", ".fseventsd",
	".TemporaryItems", ".localized", "Thumbs.db", "desktop.ini", tmpPrefix + "*",
}

// Ignored проверяет путь (через "/") по шаблонам: шаблон сопоставляется и с
// именем файла, и с полным путём, и с каждым родительским каталогом.
func (f *Folder) Ignored(name string) bool {
	f.mu.Lock()
	patterns := append([]string(nil), f.ignore...)
	f.mu.Unlock()
	return matchIgnore(patterns, name)
}

func matchIgnore(patterns []string, name string) bool {
	for p := name; p != "." && p != ""; p = path.Dir(p) {
		base := path.Base(p)
		for _, pat := range patterns {
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
		}
	}
	return false
}

type scanIO struct {
	openDir  func(*rootFS, string) (*os.File, error)
	readDir  func(*os.File) ([]os.DirEntry, error)
	info     func(*os.File, os.DirEntry) (os.FileInfo, scanStat, error)
	openRead func(*rootFS, string) (*os.File, error)
}

func (f *Folder) rootScanIO() scanIO {
	return scanIO{func(r *rootFS, p string) (*os.File, error) { return r.openDir(p, false) },
		func(d *os.File) ([]os.DirEntry, error) { return d.ReadDir(-1) },
		infoAt,
		func(r *rootFS, p string) (*os.File, error) { return r.openRead(p) }}
}

func infoAt(dir *os.File, entry os.DirEntry) (os.FileInfo, scanStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, scanStat{}, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return nil, scanStat{}, nil
	}
	meta := scanStatFromSys(&st, st.Size, uint32(st.Mode))
	return scanFileInfo{name: entry.Name(), size: st.Size, mode: fileMode(uint32(st.Mode)), mtime: time.Unix(0, meta.mtime), sys: &st}, meta, nil
}

type scanFileInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	sys   *unix.Stat_t
}

func (i scanFileInfo) Name() string       { return i.name }
func (i scanFileInfo) Size() int64        { return i.size }
func (i scanFileInfo) Mode() os.FileMode  { return i.mode }
func (i scanFileInfo) ModTime() time.Time { return i.mtime }
func (i scanFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i scanFileInfo) Sys() any           { return i.sys }

func fileMode(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFIFO:
		m |= os.ModeNamedPipe
	case unix.S_IFCHR:
		m |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFBLK:
		m |= os.ModeDevice
	case unix.S_IFSOCK:
		m |= os.ModeSocket
	}
	if mode&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}

func readIgnore(ioops scanIO, r *rootFS) ([]string, error) {
	patterns := append([]string(nil), builtinIgnore...)
	file, err := ioops.openRead(r, ignoreFile)
	if errors.Is(err, os.ErrNotExist) {
		return patterns, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, strings.TrimSuffix(line, "/"))
		}
	}
	err = sc.Err()
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return patterns, nil
}

func stableHash(file *os.File) (string, os.FileInfo, scanStat, error) {
	before, err := file.Stat()
	if err != nil {
		return "", nil, scanStat{}, err
	}
	if !before.Mode().IsRegular() {
		return "", nil, scanStat{}, errors.New("scan: file is no longer regular")
	}
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", nil, scanStat{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", nil, scanStat{}, err
	}
	meta := scanStatFromInfo(after)
	if scanStatFromInfo(before) != meta {
		return "", nil, scanStat{}, errors.New("scan: file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), after, meta, nil
}

func (f *Folder) Scan() error { return f.scanWith(f.rootScanIO()) }

// scanWith builds a complete candidate before touching published files or clocks.
func (f *Folder) scanWith(ioops scanIO) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	// The persistent latch is checked before opening or walking the root. A
	// returning mount or a successful later scan cannot implicitly confirm it.
	if err := f.guardRoot(); err != nil {
		return err
	}
	r, err := openRootFS(f.Root)
	if err != nil {
		return err
	}
	defer r.Close()
	patterns, err := readIgnore(ioops, r)
	if err != nil {
		return err
	}
	candidate := map[string]*FileInfo{}
	f.mu.Lock()
	indexed := make(map[string]bool, len(f.files))
	for name, fi := range f.files {
		if !fi.Deleted {
			indexed[name] = true
		}
	}
	cached := make(map[string]scanHashCache, len(f.hashCache))
	for disk, entry := range f.hashCache {
		cached[disk] = entry
	}
	f.mu.Unlock()
	newCache := make(map[string]scanHashCache)
	canonicalDisk := make(map[string]string)
	var walk func(string) error
	walk = func(dir string) error {
		d, err := ioops.openDir(r, dir)
		if err != nil {
			return err
		}
		defer d.Close()
		entries, err := ioops.readDir(d)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			nameOnDisk := entry.Name()
			disk := nameOnDisk
			if dir != "." {
				disk = dir + "/" + nameOnDisk
			}
			name := norm.NFC.String(disk)
			if !validName(name) {
				return errors.New("scan: invalid canonical name: " + disk)
			}
			if prior, ok := canonicalDisk[name]; ok && prior != disk {
				return errors.New("scan: canonical name collision: " + prior + " and " + disk)
			}
			canonicalDisk[name] = disk
			info, meta, err := ioops.info(d, entry)
			if err != nil {
				return err
			}
			if info == nil {
				if indexed[name] {
					return errors.New("scan: indexed file changed type: " + name)
				}
				continue
			}
			if matchIgnore(patterns, name) {
				continue
			}
			if info.IsDir() {
				if err := walk(disk); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				if indexed[name] {
					return errors.New("scan: indexed file changed type: " + name)
				}
				continue
			}
			hash, stable := "", info
			if entry, ok := cached[disk]; ok && entry.scanStat == meta {
				hash = entry.hash
			} else {
				file, err := ioops.openRead(r, disk)
				if err != nil {
					return err
				}
				var stableMeta scanStat
				hash, stable, stableMeta, err = stableHash(file)
				closeErr := file.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
				if stableMeta != meta {
					return errors.New("scan: entry changed before hashing: " + disk)
				}
			}
			newCache[disk] = scanHashCache{scanStat: meta, hash: hash}
			fi := &FileInfo{Name: name, Size: stable.Size(), ModTime: stable.ModTime().UnixNano(), Exec: stable.Mode().Perm()&0100 != 0, Hash: hash}
			if disk != name {
				fi.Disk = disk
			}
			candidate[name] = fi
		}
		return nil
	}
	if err := walk("."); err != nil {
		return err
	}
	// Recheck after the complete walk so a root replacement during scanning
	// cannot publish a candidate collected from a stale directory descriptor.
	if err := f.guardRoot(); err != nil {
		return err
	}
	f.mu.Lock()
	hasIndexedEntries := len(f.files) != 0 // tombstones also require confirmation
	f.mu.Unlock()
	if len(candidate) == 0 && hasIndexedEntries {
		return f.blockRoot("root_empty", "scan found no regular files in a nonempty index")
	}
	return f.commitScan(candidate, patterns, newCache)
}

func (f *Folder) commitScan(found map[string]*FileInfo, patterns []string, hashCache map[string]scanHashCache) error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	next := make(map[string]*FileInfo, len(f.files)+len(found))
	for k, v := range f.files {
		c := *v
		c.Version = v.Version.Copy()
		next[k] = &c
	}
	clock := f.clock
	tick := func(v Vector) uint64 {
		for _, n := range v {
			if n > clock {
				clock = n
			}
		}
		clock++
		return clock
	}
	changed := 0
	dirty := false
	for name, fi := range found {
		old := f.files[name]
		if old == nil || old.Deleted || old.Size != fi.Size || old.ModTime != fi.ModTime || old.Exec != fi.Exec || old.Hash != fi.Hash || old.Disk != fi.Disk {
			dirty = true
		}
		if old != nil && !old.Deleted && old.Hash == fi.Hash && old.Exec == fi.Exec {
			fi.Version = old.Version.Copy()
		} else {
			if old != nil {
				fi.Version = old.Version.Copy()
			} else {
				fi.Version = Vector{}
			}
			fi.Version[f.me] = tick(fi.Version)
			changed++
		}
		next[name] = fi
	}
	for name, fi := range f.files {
		if fi.Deleted || found[name] != nil || matchIgnore(patterns, name) {
			continue
		}
		v := fi.Version.Copy()
		v[f.me] = tick(v)
		next[name] = &FileInfo{Name: name, Deleted: true, ModTime: time.Now().UnixNano(), Version: v}
		changed++
		dirty = true
	}
	var p persisted
	if dirty {
		p = persisted{Generation: f.generation + 1, Clock: clock, Files: make([]*FileInfo, 0, len(next))}
		for _, fi := range next {
			c := *fi
			c.Version = fi.Version.Copy()
			p.Files = append(p.Files, &c)
		}
	}
	f.mu.Unlock()
	if dirty {
		f.generation++
		if err := f.persistLocked(p); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.files, f.clock, f.ignore, f.hashCache = next, clock, append([]string(nil), patterns...), hashCache
	if changed > 0 {
		f.notify()
	}
	f.lastScan = time.Now()
	f.mu.Unlock()
	if changed > 0 && f.log != nil {
		f.log.Printf("скан: локальных изменений: %d", changed)
	}
	return nil
}
