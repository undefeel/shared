package folder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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

func (f *Folder) loadIgnore() {
	patterns := append([]string(nil), builtinIgnore...)
	if file, err := os.Open(filepath.Join(f.Root, ignoreFile)); err == nil {
		sc := bufio.NewScanner(file)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, strings.TrimSuffix(line, "/"))
			}
		}
		file.Close()
	}
	f.mu.Lock()
	f.ignore = patterns
	f.mu.Unlock()
}

// Ignored проверяет путь (через "/") по шаблонам: шаблон сопоставляется и с
// именем файла, и с полным путём, и с каждым родительским каталогом.
func (f *Folder) Ignored(name string) bool {
	f.mu.Lock()
	patterns := f.ignore
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

func hashFile(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Scan обходит папку и обновляет индекс: новые и изменённые файлы получают
// новую версию, пропавшие помечаются удалёнными.
func (f *Folder) Scan() error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.loadIgnore()

	seen := map[string]bool{}
	changed := 0
	err := filepath.WalkDir(f.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == f.Root {
				return err
			}
			f.log.Printf("скан: %v", err)
			return nil
		}
		if p == f.Root {
			return nil
		}
		rel, _ := filepath.Rel(f.Root, p)
		disk := filepath.ToSlash(rel)
		name := norm.NFC.String(disk)
		if f.Ignored(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // каталоги обходим, симлинки и прочее пропускаем
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seen[name] = true

		f.mu.Lock()
		old := f.files[name]
		unchanged := old != nil && !old.Deleted && old.Size == info.Size() && old.ModTime == info.ModTime().UnixNano()
		f.mu.Unlock()
		if unchanged {
			return nil
		}

		hash, err := hashFile(p)
		if err != nil {
			f.log.Printf("скан: %v", err)
			return nil
		}
		fi := &FileInfo{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
			Exec:    info.Mode().Perm()&0o100 != 0,
			Hash:    hash,
		}
		if disk != name {
			fi.Disk = disk
		}
		f.mu.Lock()
		if old != nil && !old.Deleted && old.Hash == hash {
			fi.Version = old.Version // содержимое то же, поменялись только атрибуты
		} else {
			fi.Version = Vector{}
			if old != nil {
				fi.Version = old.Version.Copy()
			}
			fi.Version[f.me] = f.tick(fi.Version)
			changed++
		}
		f.files[name] = fi
		f.mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}

	f.mu.Lock()
	for name, fi := range f.files {
		if fi.Deleted || seen[name] || matchIgnore(f.ignore, name) {
			continue
		}
		nv := fi.Version.Copy()
		nv[f.me] = f.tick(nv)
		f.files[name] = &FileInfo{Name: name, Deleted: true, ModTime: time.Now().UnixNano(), Version: nv}
		changed++
	}
	if changed > 0 {
		f.notify()
	}
	f.lastScan = time.Now()
	f.mu.Unlock()

	if changed > 0 {
		f.log.Printf("скан: локальных изменений: %d", changed)
	}
	return f.save()
}
