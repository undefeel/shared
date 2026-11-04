package folder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"reflect"
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
	info     func(*os.File, os.DirEntry) (os.FileInfo, error)
	openRead func(*rootFS, string) (*os.File, error)
}

func (f *Folder) rootScanIO() scanIO {
	return scanIO{func(r *rootFS, p string) (*os.File, error) { return r.openDir(p, false) },
		func(d *os.File) ([]os.DirEntry, error) { return d.ReadDir(-1) },
		infoAt,
		func(r *rootFS, p string) (*os.File, error) { return r.openRead(p) }}
}

func infoAt(dir *os.File, entry os.DirEntry) (os.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return nil, nil
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(dir.Fd()), entry.Name(), flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), entry.Name())
	defer file.Close()
	return file.Stat()
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

func stableHash(file *os.File) (string, os.FileInfo, error) {
	before, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if !before.Mode().IsRegular() {
		return "", nil, errors.New("scan: file is no longer regular")
	}
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	statField := func(info os.FileInfo, names ...string) any {
		st := reflect.ValueOf(info.Sys()).Elem()
		for _, name := range names {
			if field := st.FieldByName(name); field.IsValid() {
				return field.Interface()
			}
		}
		return nil
	}
	if !reflect.DeepEqual(statField(before, "Dev"), statField(after, "Dev")) ||
		!reflect.DeepEqual(statField(before, "Ino"), statField(after, "Ino")) ||
		before.Size() != after.Size() || before.Mode() != after.Mode() ||
		!before.ModTime().Equal(after.ModTime()) ||
		!reflect.DeepEqual(statField(before, "Ctim", "Ctimespec"), statField(after, "Ctim", "Ctimespec")) {
		return "", nil, errors.New("scan: file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), after, nil
}

func (f *Folder) Scan() error { return f.scanWith(f.rootScanIO()) }

// scanWith builds a complete candidate before touching published files or clocks.
func (f *Folder) scanWith(ioops scanIO) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
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
	f.mu.Unlock()
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
			info, err := ioops.info(d, entry)
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
			file, err := ioops.openRead(r, disk)
			if err != nil {
				return err
			}
			hash, stable, hashErr := stableHash(file)
			closeErr := file.Close()
			if hashErr != nil {
				return hashErr
			}
			if closeErr != nil {
				return closeErr
			}
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
	return f.commitScan(candidate, patterns)
}

func (f *Folder) commitScan(found map[string]*FileInfo, patterns []string) error {
	f.mu.Lock()
	oldFiles := f.files
	oldClock := f.clock
	oldIgnore := f.ignore
	next := make(map[string]*FileInfo, len(oldFiles)+len(found))
	for k, v := range oldFiles {
		c := *v
		c.Version = v.Version.Copy()
		next[k] = &c
	}
	clock := oldClock
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
		old := oldFiles[name]
		if old == nil || old.Deleted || old.Size != fi.Size || old.ModTime != fi.ModTime || old.Exec != fi.Exec || old.Hash != fi.Hash || old.Disk != fi.Disk {
			dirty = true
		}
		if old != nil && !old.Deleted && old.Hash == fi.Hash && old.Exec == fi.Exec {
			fi.Version = old.Version.Copy()
			if old.Size == fi.Size && old.ModTime == fi.ModTime && old.Disk == fi.Disk {
				fi.Version = old.Version.Copy()
			} else {
				fi.Version = old.Version.Copy()
			}
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
	for name, fi := range oldFiles {
		if fi.Deleted || found[name] != nil || matchIgnore(patterns, name) {
			continue
		}
		v := fi.Version.Copy()
		v[f.me] = tick(v)
		next[name] = &FileInfo{Name: name, Deleted: true, ModTime: time.Now().UnixNano(), Version: v}
		changed++
		dirty = true
	}
	// save() owns saveMu and snapshots under mu; publish only a complete candidate.
	f.files, f.clock = next, clock
	f.ignore = append([]string(nil), patterns...)
	f.mu.Unlock()
	if dirty {
		if err := f.save(); err != nil {
			f.mu.Lock()
			f.files, f.clock, f.ignore = oldFiles, oldClock, oldIgnore
			f.mu.Unlock()
			return err
		}
	}
	f.mu.Lock()
	if changed > 0 {
		f.notify()
	}
	f.lastScan = time.Now()
	f.mu.Unlock()
	return nil
}
