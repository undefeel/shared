package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fetcher скачивает содержимое файла fi у пира в w.
type Fetcher func(ctx context.Context, fi FileInfo, w io.Writer) error

type action int

const (
	skip action = iota
	take
	takeWithConflict
	mergeOnly
)

// decide определяет, что делать с удалённой записью r при локальной l.
// Решение симметрично: пир, сравнивая те же записи со своей стороны,
// приходит к зеркальному выводу, поэтому обе стороны сходятся к одной версии.
func (f *Folder) decide(l *FileInfo, r *FileInfo, peer string) action {
	if l == nil {
		return take
	}
	switch r.Version.Compare(l.Version) {
	case Equal, Lesser:
		return skip
	case Greater:
		return take
	}
	// Параллельные изменения.
	switch {
	case l.Deleted && r.Deleted:
		return mergeOnly
	case !l.Deleted && !r.Deleted && l.Hash == r.Hash:
		return mergeOnly
	case l.Deleted:
		return take // правка важнее удаления
	case r.Deleted:
		return skip
	}
	if r.ModTime > l.ModTime || (r.ModTime == l.ModTime && peer > f.me) {
		return takeWithConflict
	}
	return skip // мы победили; пир сам сохранит свою версию как конфликтную
}

// Event — что произошло с файлом при применении изменений пира.
type Event string

const (
	EventReceived Event = "in"
	EventDeleted  Event = "deleted"
	EventConflict Event = "conflict"
)

// Progress получает план и ход применения изменений пира. Может быть nil.
type Progress interface {
	Plan(files int, bytes int64)
	Done(fi FileInfo, ev Event, bytes int64)
}

type step struct {
	l   *FileInfo
	r   FileInfo
	act action
}

// needsDownload — придётся ли тянуть содержимое с пира.
func (s step) needsDownload() bool {
	return !s.r.Deleted && (s.l == nil || s.l.Deleted || s.l.Hash != s.r.Hash || s.act == takeWithConflict)
}

// ApplyRemote сверяет индекс пира с локальным и подтягивает изменения.
// Возвращает число применённых записей и число неудач (их стоит повторить).
func (f *Folder) ApplyRemote(ctx context.Context, peer string, remote []FileInfo, fetch Fetcher, progress Progress) (applied, failed int) {
	defer func() {
		if applied > 0 {
			if err := f.save(); err != nil {
				f.log.Printf("сохранение индекса: %v", err)
			}
		}
	}()

	var steps []step
	var files int
	var bytes int64
	for i := range remote {
		r := remote[i]
		r.Disk = ""
		if !validName(r.Name) {
			f.log.Printf("%s: пропущен недопустимый путь %q", peer, r.Name)
			failed++
			continue
		}
		if f.Ignored(r.Name) {
			continue
		}
		f.mu.Lock()
		var l *FileInfo
		if cur, ok := f.files[r.Name]; ok {
			c := *cur
			l = &c
		}
		f.mu.Unlock()
		act := f.decide(l, &r, peer)
		if act == skip {
			continue
		}
		st := step{l, r, act}
		steps = append(steps, st)
		if act != mergeOnly {
			files++
		}
		if st.needsDownload() {
			bytes += r.Size
		}
	}
	if progress != nil && files > 0 {
		progress.Plan(files, bytes)
	}

	for _, st := range steps {
		if ctx.Err() != nil {
			return applied, failed + 1
		}
		if err := f.applyOne(ctx, st, fetch); err != nil {
			f.log.Printf("%s: %s: %v", peer, st.r.Name, err)
			failed++
			continue
		}
		applied++
		if progress != nil && st.act != mergeOnly {
			ev, n := EventReceived, int64(0)
			switch {
			case st.r.Deleted:
				ev = EventDeleted
			case st.act == takeWithConflict:
				ev = EventConflict
			}
			if st.needsDownload() {
				n = st.r.Size
			}
			progress.Done(st.r, ev, n)
		}
	}
	return applied, failed
}

var errLocalChanged = errors.New("файл изменён локально, повторим после сканирования")

func (f *Folder) applyOne(ctx context.Context, st step, fetch Fetcher) error {
	l, r, act := st.l, st.r, st.act
	if act == mergeOnly {
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := f.files[r.Name]
		if cur == nil || cur.Version.Compare(l.Version) != Equal {
			return errLocalChanged
		}
		cur.Version = cur.Version.Merge(r.Version)
		f.notify()
		return nil
	}

	var tmp string
	if st.needsDownload() {
		var err error
		if tmp, err = f.download(ctx, r, fetch); err != nil {
			return err
		}
		defer os.Remove(tmp) // после успешного rename файла уже не будет
	}

	f.scanMu.Lock()
	defer f.scanMu.Unlock()

	if l != nil && !validDisk(r.Name, l.Disk) {
		return errors.New("invalid local disk path in index")
	}
	rel := filepath.FromSlash(r.Name)
	if l != nil {
		rel = l.diskPath()
	}
	target := filepath.Join(f.Root, rel)
	if !diskMatches(target, l) {
		return errLocalChanged
	}

	switch {
	case act == takeWithConflict:
		cp := conflictName(target, f.me, time.Unix(0, l.ModTime))
		if err := os.Rename(target, cp); err != nil {
			return err
		}
		f.log.Printf("конфликт: локальная версия сохранена как %s", filepath.Base(cp))
	case r.Deleted:
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		f.removeEmptyParents(filepath.Dir(target))
	}

	if tmp != "" {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if r.Exec {
			mode = 0o755
		}
		if err := os.Chmod(tmp, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
	}

	if !r.Deleted {
		mt := time.Unix(0, r.ModTime)
		if err := os.Chtimes(target, mt, mt); err != nil {
			return err
		}
		// Запоминаем то, что реально легло на диск, иначе следующий скан
		// примет разницу в точности mtime за локальное изменение.
		st, err := os.Stat(target)
		if err != nil {
			return err
		}
		r.Size = st.Size()
		r.ModTime = st.ModTime().UnixNano()
		if l != nil {
			r.Disk = l.Disk
		}
	}

	f.mu.Lock()
	f.files[r.Name] = &r
	for _, n := range r.Version {
		f.clock = max(f.clock, n)
	}
	f.notify()
	f.mu.Unlock()
	return nil
}

// diskMatches проверяет, что файл на диске всё ещё соответствует записи
// индекса l, т.е. его не изменили после последнего скана.
func diskMatches(target string, l *FileInfo) bool {
	st, err := os.Stat(target)
	if l == nil || l.Deleted {
		return errors.Is(err, fs.ErrNotExist)
	}
	if err != nil {
		return false
	}
	return st.Size() == l.Size && st.ModTime().UnixNano() == l.ModTime
}

func (f *Folder) download(ctx context.Context, r FileInfo, fetch Fetcher) (string, error) {
	tmp, err := os.CreateTemp(f.Root, tmpPrefix+"*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = fetch(ctx, r, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != r.Hash {
		err = fmt.Errorf("хеш не совпал (файл менялся во время передачи)")
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// conflictName: notes.txt -> notes.sync-conflict-20261103-114500-ABCDEFG.txt
func conflictName(p, dev string, t time.Time) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	return fmt.Sprintf("%s.sync-conflict-%s-%s%s", base, t.Format("20060102-150405"), dev, ext)
}

// removeEmptyParents удаляет опустевшие после удаления файла каталоги,
// считая пустыми и те, где остался только мусор вроде .DS_Store.
func (f *Folder) removeEmptyParents(dir string) {
	for dir != f.Root && strings.HasPrefix(dir, f.Root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !matchIgnore(builtinIgnore, e.Name()) {
				return
			}
		}
		for _, e := range entries {
			os.Remove(filepath.Join(dir, e.Name()))
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
