package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path"
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
	case !l.Deleted && !r.Deleted && l.Hash == r.Hash && l.Exec == r.Exec:
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
	generation := f.ConfirmedGeneration()

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
			c.Version = cur.Version.Copy()
			l = &c
		}
		f.mu.Unlock()
		act := f.decide(l, &r, peer)
		if act == skip && !(l != nil && r.Deleted && !l.Deleted && r.Version.Compare(l.Version) == Concurrent) {
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
		final, err := f.applyOne(ctx, peer, generation, st, fetch)
		if err != nil {
			f.log.Printf("%s: %s: %v", peer, st.r.Name, err)
			failed++
			continue
		}
		if final.act == skip {
			continue
		}
		st = final
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

// applyOne treats step as a forecast only. Network I/O never holds scanMu.
func (f *Folder) applyOne(ctx context.Context, peer string, generation uint64, st step, fetch Fetcher) (step, error) {
	r := st.r
	if e := f.Blocked(); e != nil {
		return st, e
	}
	var tmp string
	if st.act != mergeOnly && st.needsDownload() {
		var err error
		tmp, err = f.download(ctx, r, fetch)
		if err != nil {
			return st, err
		}
	}
	downloaded := tmp
	defer func() {
		if downloaded != "" {
			_ = f.root.unlink(downloaded, false)
		}
	}()
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return st, err
	}
	if generation != f.ConfirmedGeneration() {
		return st, errLocalChanged
	}
	if err := f.guardRoot(); err != nil {
		return st, err
	}
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	next := make(map[string]*FileInfo, len(f.files)+1)
	for k, v := range f.files {
		c := *v
		c.Version = v.Version.Copy()
		next[k] = &c
	}
	clock := f.clock
	f.mu.Unlock()
	l := next[r.Name]
	disk := r.Name
	if l != nil {
		disk = l.diskPath()
	}
	if !validDisk(r.Name, disk) {
		return st, errLocalChanged
	}
	// Register an external edit before deciding against the incoming vector.
	var local *os.File
	var info os.FileInfo
	if l != nil && !l.Deleted {
		var err error
		local, err = f.root.openRead(disk)
		if err != nil {
			return st, err
		}
		defer local.Close()
		hash, actual, _, err := stableHash(local)
		if err != nil {
			return st, err
		}
		info = actual
		if hash != l.Hash || (actual.Mode().Perm()&0100 != 0) != l.Exec {
			c := *l
			c.Hash = hash
			c.Size = actual.Size()
			c.ModTime = actual.ModTime().UnixNano()
			c.Exec = actual.Mode().Perm()&0100 != 0
			clock++
			c.Version = l.Version.Copy()
			c.Version[f.me] = clock
			l = &c
			next[r.Name] = l
			if err := f.commitApply(next, clock, nil); err != nil {
				return st, err
			}
			// The first commit published next: detach before staging another.
			detached := make(map[string]*FileInfo, len(next))
			for k, v := range next {
				c := *v
				c.Version = v.Version.Copy()
				detached[k] = &c
			}
			next = detached
			l = next[r.Name]
		}
	}
	act := f.decide(l, &r, peer)
	st = step{l, r, act}
	if act == skip {
		// Concurrent edit/delete converges by remembering both vectors.
		if l != nil && r.Deleted && !l.Deleted && r.Version.Compare(l.Version) == Concurrent {
			c := *l
			c.Version = l.Version.Merge(r.Version)
			next[r.Name] = &c
			return st, f.commitApply(next, clock, nil)
		}
		return st, nil
	}
	if act == mergeOnly {
		c := *l
		c.Version = l.Version.Merge(r.Version)
		next[r.Name] = &c
		return st, f.commitApply(next, clock, nil)
	}
	if st.needsDownload() && tmp == "" {
		return st, errLocalChanged
	}
	parent, leaf, err := f.root.parent(disk, true)
	if err != nil {
		return st, err
	}
	defer parent.Close()
	r.Disk = ""
	if disk != r.Name {
		r.Disk = disk
	}
	var incoming *os.File
	if !r.Deleted {
		// Stage in the target parent so intent/recovery names remain siblings.
		staged := path.Join(path.Dir(disk), tmpPrefix+captureToken())
		incoming, err = f.root.create(staged)
		if err != nil {
			return st, err
		}
		defer incoming.Close()
		defer func() {
			if f.Blocked() == nil {
				_ = f.root.unlink(staged, false)
			}
		}()
		var source *os.File
		if tmp != "" {
			source, err = f.root.openRead(tmp)
		} else {
			source = local
			_, err = source.Seek(0, 0)
		}
		if err != nil {
			return st, err
		}
		_, err = io.Copy(incoming, source)
		if tmp != "" {
			source.Close()
		}
		if err != nil {
			return st, err
		}
		mode := os.FileMode(0644)
		if r.Exec {
			mode = 0755
		}
		if err = incoming.Chmod(mode); err != nil {
			return st, err
		}
		times := []unix.Timespec{unix.NsecToTimespec(r.ModTime), unix.NsecToTimespec(r.ModTime)}
		if err = unix.Futimes(int(incoming.Fd()), []unix.Timeval{unix.NsecToTimeval(times[0].Nano()), unix.NsecToTimeval(times[1].Nano())}); err != nil {
			return st, err
		}
		actual, e := incoming.Stat()
		if e != nil {
			return st, e
		}
		r.ModTime = actual.ModTime().UnixNano()
		r.Size = actual.Size()
		tmp = staged
	}
	tree := &commitTree{incoming: incoming, dirs: []*os.File{parent}, intent: journalIntent{TxID: captureToken(), Target: disk, Tmp: tmp, Binding: f.binding, Before: l, After: &r}}
	for dir := path.Dir(disk); dir != "."; dir = path.Dir(dir) {
		ancestor, e := f.root.openDir(path.Dir(dir), false)
		if e != nil {
			return st, e
		}
		defer ancestor.Close()
		tree.dirs = append(tree.dirs, ancestor)
	}
	if r.Deleted {
		tree.intent.Tmp = ""
	}
	if local != nil {
		c, e := f.root.prepareCapture(disk)
		if e != nil {
			return st, e
		}
		defer c.Close()
		tree.capture = c
		tree.intent.Capture = c.Recovery
		meta := scanStatFromInfo(info)
		tree.intent.Identity = captureIdentity{meta.dev, meta.ino}
	}
	tree.mutate = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tree.capture != nil {
			captured, e := openRegularAt(parent, path.Base(tree.capture.Recovery), unix.O_RDONLY, 0)
			if e != nil {
				return e
			}
			hash, actual, _, e := stableHash(captured)
			captured.Close()
			if e != nil {
				return e
			}
			if !os.SameFile(info, actual) || hash != l.Hash || (actual.Mode().Perm()&0100 != 0) != l.Exec {
				c := *l
				c.Version = l.Version.Copy()
				c.Version[f.me] = clock
				c.Hash, c.Size, c.ModTime, c.Exec = hash, actual.Size(), actual.ModTime().UnixNano(), actual.Mode().Perm()&0100 != 0
				act = f.decide(&c, &r, peer)
				if act == skip || r.Deleted {
					restored, restoreErr := tree.capture.restore(f.me, hash)
					if restoreErr != nil {
						return restoreErr
					}
					if restored != disk {
						return errLocalChanged
					}
					c.Version = c.Version.Merge(r.Version)
					*next[r.Name] = c
					st = step{&c, c, skip}
					tree.discard = func() error { return nil }
					return nil
				}
				l = &c
				r.Version = r.Version.Merge(c.Version)
				st = step{l, r, act}
			}
			if act == takeWithConflict {
				if _, e = tree.capture.promote(f.me, l.Hash); e != nil {
					return e
				}
				tree.discard = func() error { return nil }
			}
		}
		// Recheck the root and parent after capture, before installation.
		root, e := openRootFS(f.Root)
		if e != nil {
			return e
		}
		binding, e := bindingOf(root, f.Root)
		root.Close()
		if e != nil {
			return e
		}
		if binding != f.binding {
			return errLocalChanged
		}
		// Verify pinned parent still belongs to the current rooted path.
		current, _, e := f.root.parent(disk, false)
		if e != nil {
			return e
		}
		defer current.Close()
		a, e := current.Stat()
		if e != nil {
			return e
		}
		b, e := parent.Stat()
		if e != nil {
			return e
		}
		if !os.SameFile(a, b) {
			return errLocalChanged
		}
		if r.Deleted {
			return nil
		}
		return renameNoReplace(parent, path.Base(tmp), parent, leaf)
	}
	if act == takeWithConflict || (l != nil && l.Deleted && !r.Deleted) {
		r.Version = r.Version.Merge(l.Version)
	}
	next[r.Name] = &r
	for _, n := range r.Version {
		clock = max(clock, n)
	}
	clock++ // Reserve a local tick for an edit discovered after capture.
	st.r = r
	err = f.commitApply(next, clock, tree)
	if err == nil && r.Deleted {
		for dir := path.Dir(disk); dir != "."; dir = path.Dir(dir) {
			if e := f.root.unlink(dir, true); e != nil {
				break
			}
			p, _, e := f.root.parent(dir, false)
			if e != nil {
				return st, e
			}
			e = syncDir(p)
			p.Close()
			if e != nil {
				return st, e
			}
		}
	}
	return st, err
}

// commitApply requires scanMu and saveMu; published records are immutable.
func (f *Folder) commitApply(next map[string]*FileInfo, clock uint64, tree *commitTree) error {
	p := persisted{Generation: f.generation + 1, Clock: clock}
	for _, fi := range next {
		p.Files = append(p.Files, fi) // next is private until durable publication
	}
	return f.commitLocked(p, tree, func() { f.files = next; f.clock = clock; f.notify() })
}

func (f *Folder) download(ctx context.Context, r FileInfo, fetch Fetcher) (string, error) {
	name := tmpPrefix + captureToken()
	tmp, err := f.root.create(name)
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
		_ = f.root.unlink(name, false)
		return "", err
	}
	return name, nil
}
