package folder

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

type scanIO struct {
	openDir      func(*rootFS, string) (*os.File, error)
	readDir      func(*os.File) ([]os.DirEntry, error)
	info         func(*os.File, os.DirEntry) (os.FileInfo, scanStat, error)
	openRead     func(*rootFS, string) (*os.File, error)
	beforeCommit func()
}

var ErrScanPending = errors.New("scan pending: files are not stable")

func (f *Folder) rootScanIO() scanIO {
	return scanIO{func(r *rootFS, p string) (*os.File, error) { return r.openDir(p, false) },
		func(d *os.File) ([]os.DirEntry, error) { return d.ReadDir(-1) },
		infoAt,
		func(r *rootFS, p string) (*os.File, error) { return r.openRead(p) }, nil}
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
	var patterns []string
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

// ScanError is transient and clears after a successful full scan.
type ScanError struct {
	Message string    `json:"message"`
	Path    string    `json:"path"`
	Time    time.Time `json:"time"`
}

func (f *Folder) LastScanError() *ScanError {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scanError == nil {
		return nil
	}
	copy := *f.scanError
	return &copy
}

func (f *Folder) Scan() error { return f.scanWith(f.rootScanIO()) }

// scanWith builds a complete candidate before touching published files or clocks.
func (f *Folder) scanWith(ioops scanIO) error {
	if err := f.preflightStability(ioops); err != nil {
		f.recordScanResult(err)
		return err
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	return f.scanLocked(ioops, false)
}

// preflightStability observes metadata outside scanMu and waits at most once.
// A separate root descriptor pins the traversal while other scans can proceed.
func (f *Folder) preflightStability(ioops scanIO) error {
	f.scanMu.Lock()
	if f.root == nil {
		f.scanMu.Unlock()
		return errors.New("scan: root unavailable")
	}
	if err := f.guardRoot(); err != nil {
		f.scanMu.Unlock()
		return err
	}
	r, err := ioops.openDir(f.root, ".")
	f.scanMu.Unlock()
	if err != nil {
		f.blockRoot("root_unavailable", err.Error())
		return err
	}
	root := &rootFS{root: r}
	defer root.Close()
	patterns, err := readIgnore(ioops, root)
	if err != nil {
		return err
	}
	probes, err := collectScanProbes(ioops, root, patterns)
	if err != nil {
		return err
	}
	tracker := f.integrationStability()
	pending := false
	for name, meta := range probes {
		if !tracker.probe(name, meta) {
			pending = true
		}
	}
	if !pending {
		return nil
	}
	timer := time.NewTimer(time.Second)
	<-timer.C
	probes, err = collectScanProbes(ioops, root, patterns)
	if err != nil {
		return err
	}
	for name, meta := range probes {
		if !tracker.probe(name, meta) {
			return ErrScanPending
		}
	}
	return nil
}

func (f *Folder) integrationStability() *stabilityTracker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stability == nil {
		f.stability = newStabilityTracker()
	}
	return f.stability
}

func collectScanProbes(ioops scanIO, r *rootFS, patterns []string) (map[string]scanStat, error) {
	probes := make(map[string]scanStat)
	canonical := make(map[string]string)
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
			disk := entry.Name()
			if dir != "." {
				disk = dir + "/" + disk
			}
			name := norm.NFC.String(disk)
			if !validName(name) {
				return errors.New("scan: invalid canonical name: " + disk)
			}
			if prior, ok := canonical[name]; ok && prior != disk {
				return errors.New("scan: canonical name collision: " + prior + " and " + disk)
			}
			canonical[name] = disk
			info, meta, err := ioops.info(d, entry)
			if err != nil {
				return err
			}
			if info == nil || ignoredWith(patterns, name) {
				continue
			}
			if info.IsDir() {
				if err := walk(disk); err != nil {
					return err
				}
				continue
			}
			if info.Mode().IsRegular() {
				probes[name] = meta
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return probes, nil
}

func (f *Folder) scanLocked(ioops scanIO, allowEmpty bool) (result error) {
	scanPath := f.Root
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if result == nil {
			f.scanError = nil
			return
		}
		var root *RootError
		if errors.As(result, &root) {
			return
		}
		now := time.Now()
		previous := f.scanError
		if previous == nil || previous.Message != result.Error() || now.Sub(f.scanLogTime) >= 5*time.Minute {
			if f.log != nil {
				f.log.Printf("скан %s: %v", scanPath, result)
			}
			f.scanLogTime = now
		}
		f.scanError = &ScanError{Message: result.Error(), Path: scanPath, Time: now}
	}()

	// The persistent latch is checked before opening or walking the root. A
	// returning mount or a successful later scan cannot implicitly confirm it.
	if err := f.guardRoot(); err != nil {
		return err
	}
	// Walk from the root descriptor pinned during Open. Reopening the configured
	// pathname here could silently switch the candidate to a replacement root.
	r := f.root
	scanPath = ignoreFile
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
		scanPath = dir
		d, err := ioops.openDir(r, dir)
		if err != nil {
			if dir == "." {
				return f.blockRoot("root_unavailable", err.Error())
			}
			return err
		}
		defer d.Close()
		entries, err := ioops.readDir(d)
		if err != nil {
			if dir == "." {
				return f.blockRoot("root_unavailable", err.Error())
			}
			return err
		}
		for _, entry := range entries {
			nameOnDisk := entry.Name()
			disk := nameOnDisk
			if dir != "." {
				disk = dir + "/" + nameOnDisk
			}
			scanPath = disk
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
			if ignoredWith(patterns, name) {
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
			if !allowEmpty && !f.integrationStability().probe(name, meta) {
				return ErrScanPending
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
	if ioops.beforeCommit != nil {
		ioops.beforeCommit()
	}
	if err := f.commitScan(candidate, patterns, newCache, allowEmpty); err != nil {
		return err
	}
	present := make(map[string]struct{}, len(candidate))
	for name := range candidate {
		present[name] = struct{}{}
	}
	f.integrationStability().reconcile(present)
	return nil
}

func (f *Folder) commitScan(found map[string]*FileInfo, patterns []string, hashCache map[string]scanHashCache, allowEmpty bool) error {
	// Keep the final binding check in the commit path: a root replaced after
	// the walk must not publish a candidate collected from another directory.
	// scanMu is held by Scan; guardRoot persists any resulting latch itself.
	if err := f.guardRoot(); err != nil {
		return err
	}
	f.saveMu.Lock()
	f.mu.Lock()
	next := make(map[string]*FileInfo, len(f.files)+len(found))
	for k, v := range f.files {
		c := *v
		c.Version = v.Version.Copy()
		next[k] = &c
	}
	// Decide against the same current files snapshot used to build next. Release
	// both locks before blockRoot persists the latch through saveMu.
	visibleExisting := false
	for name := range f.files {
		if !ignoredWith(patterns, name) {
			visibleExisting = true
			break
		}
	}
	if !allowEmpty && len(found) == 0 && visibleExisting {
		f.mu.Unlock()
		f.saveMu.Unlock()
		return f.blockRoot("root_empty", "scan found no regular files in a nonempty index")
	}
	defer f.saveMu.Unlock()
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
		if fi.Deleted || found[name] != nil || ignoredWith(patterns, name) {
			continue
		}
		v := fi.Version.Copy()
		v[f.me] = tick(v)
		next[name] = &FileInfo{Name: name, Deleted: true, ModTime: time.Now().UnixNano(), Version: v}
		changed++
		dirty = true
	}
	var p persisted
	ignoreChanged := !equalStrings(f.ignore, patterns)
	if ignoreChanged {
		dirty = true
	}
	if dirty {
		p = persisted{Generation: f.generation + 1, Clock: clock, Files: make([]*FileInfo, 0, len(next))}
		for _, fi := range next {
			c := *fi
			c.Version = fi.Version.Copy()
			p.Files = append(p.Files, &c)
		}
	}
	f.mu.Unlock()
	publish := func() {
		f.files, f.clock, f.ignore, f.hashCache = next, clock, append([]string(nil), patterns...), hashCache
		if changed > 0 || ignoreChanged {
			f.notify()
		}
		f.lastScan = time.Now()
	}
	if dirty {
		if err := f.commitLocked(p, nil, publish); err != nil {
			return err
		}
	} else {
		f.mu.Lock()
		publish()
		f.mu.Unlock()
	}
	if changed > 0 && f.log != nil {
		f.log.Printf("скан: локальных изменений: %d", changed)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *Folder) recordScanResult(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		f.scanError = nil
		return
	}
	var root *RootError
	if errors.As(err, &root) {
		return
	}
	now := time.Now()
	f.scanError = &ScanError{Message: err.Error(), Path: f.Root, Time: now}
}

// ScanGeneration rejects events queued by a canceled watcher generation.
func (f *Folder) ScanGeneration(ctx context.Context, generation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation != f.ConfirmedGeneration() {
		return errors.New("stale watcher generation")
	}
	if err := f.preflightStability(f.rootScanIO()); err != nil {
		f.recordScanResult(err)
		return err
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation != f.ConfirmedGeneration() {
		return errors.New("stale watcher generation")
	}
	return f.scanLocked(f.rootScanIO(), false)
}
