package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"sync"
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
		r.StoreDeletedAt = 0
		if !r.Deleted && (r.Size < 0 || !validSHA256(r.Hash)) {
			f.log.Printf("%s: пропущена некорректная запись %q (размер или SHA-256)", peer, r.Name)
			failed++
			continue
		}
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
		if st.needsDownload() && r.Size > math.MaxInt64-bytes {
			f.log.Printf("%s: переполнение размера плана для %q", peer, r.Name)
			failed++
			continue
		}
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

	// Keep enough operations in flight to fill a group; result reporting remains
	// serialized because Progress implementations need not be concurrent-safe.
	type result struct {
		st  step
		err error
	}
	results := make(chan result, groupCount)
	var wg sync.WaitGroup
	for start := 0; start < len(steps); start += groupCount {
		end := min(start+groupCount, len(steps))
		for _, st := range steps[start:end] {
			wg.Add(1)
			go func(st step) {
				defer wg.Done()
				final, err := f.applyOne(ctx, peer, generation, st, fetch)
				results <- result{final, err}
			}(st)
		}
		for range steps[start:end] {
			res := <-results
			st := res.st
			if res.err != nil {
				f.log.Printf("%s: %s: %v", peer, st.r.Name, res.err)
				failed++
				continue
			}
			if st.act == skip {
				continue
			}
			applied++
			if progress != nil && st.act != mergeOnly {
				ev, n := EventReceived, int64(0)
				if st.r.Deleted {
					ev = EventDeleted
				} else if st.act == takeWithConflict {
					ev = EventConflict
				}
				if st.needsDownload() {
					n = st.r.Size
				}
				progress.Done(st.r, ev, n)
			}
		}
		wg.Wait()
	}
	return applied, failed
}

var errLocalChanged = errors.New("файл изменён локально, повторим после сканирования")

// applyOne treats step as a forecast only. Network I/O never holds scanMu.
func (f *Folder) applyOne(ctx context.Context, peer string, generation uint64, st step, fetch Fetcher) (step, error) {
	if e := f.Blocked(); e != nil {
		return st, e
	}
	prepared, err := f.stageApply(ctx, st, fetch)
	if err != nil {
		return st, err
	}
	defer prepared.close(f)
	return f.submitApply(ctx, peer, generation, st, prepared)
}

func (f *Folder) applyStaged(ctx context.Context, peer string, generation uint64, st step, prepared *stagedApply) (step, error) {
	return f.prepareApply(ctx, peer, generation, st, prepared, nil)
}

func (f *Folder) prepareApply(ctx context.Context, peer string, generation uint64, st step, prepared *stagedApply, batch *applyBatch) (step, error) {
	r := st.r
	tmp := prepared.tmp
	if batch == nil {
		f.scanMu.Lock()
		defer f.scanMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	if generation != f.ConfirmedGeneration() {
		return st, errLocalChanged
	}
	if err := f.guardRootWithSaveLock(batch != nil); err != nil {
		return st, err
	}
	if batch == nil {
		f.saveMu.Lock()
		defer f.saveMu.Unlock()
	}
	var next map[string]*FileInfo
	var clock uint64
	if batch != nil {
		next, clock = batch.next, batch.clock
	} else {
		f.mu.Lock()
		next = copyApplyFiles(f.files)
		clock = f.clock
		f.mu.Unlock()
	}
	save := func(tree *commitTree) error {
		if batch == nil {
			return f.commitApply(next, clock, tree)
		}
		batch.clock = clock
		if tree != nil {
			batch.trees = append(batch.trees, tree)
		}
		return nil
	}
	var deferLater []func()
	defer func() {
		for i := len(deferLater) - 1; i >= 0; i-- {
			deferLater[i]()
		}
	}()
	cleanup := func(fn func()) {
		if batch == nil {
			deferLater = append(deferLater, fn)
		} else {
			batch.cleanup = append(batch.cleanup, fn)
		}
	}
	l := next[r.Name]
	disk := r.Name
	if l != nil {
		disk = l.diskPath()
	}
	if !validDisk(r.Name, disk) {
		return st, errLocalChanged
	}
	// Register an external edit before deciding against the incoming vector.
	local, info := prepared.local, prepared.info
	if disk != prepared.disk || (l != nil && !l.Deleted) != (local != nil) {
		return st, errLocalChanged
	}
	if local != nil {
		// Only stat under scanMu; the bytes were verified during staging.
		current, err := f.root.openRead(disk)
		if err != nil {
			return st, err
		}
		actual, err := current.Stat()
		current.Close()
		if err != nil {
			return st, err
		}
		if scanStatFromInfo(actual) != scanStatFromInfo(info) {
			return st, errLocalChanged
		}
		hash := prepared.hash
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
			if err := save(nil); err != nil {
				return st, err
			}
			if batch == nil {
				next = copyApplyFiles(next)
				l = next[r.Name]
			}
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
			return st, save(nil)
		}
		return st, nil
	}
	if act == mergeOnly {
		c := *l
		c.Version = l.Version.Merge(r.Version)
		next[r.Name] = &c
		return st, save(nil)
	}
	if st.needsDownload() && tmp == "" {
		return st, errLocalChanged
	}
	parent, leaf, err := f.root.parent(disk, true)
	if err != nil {
		return st, err
	}
	cleanup(func() { parent.Close() })
	r.Disk = ""
	r.StoreDeletedAt = 0
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
		cleanup(func() { incoming.Close() })
		cleanup(func() {
			if _, e := os.Stat(f.intentPath()); os.IsNotExist(e) {
				_ = f.root.unlink(staged, false)
			}
		})
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
		var localMode os.FileMode
		if info != nil {
			localMode = info.Mode().Perm()
		}
		mode := incomingMode(localMode, local != nil, r.Exec)
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
		cleanup(func() { ancestor.Close() })
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
		cleanup(func() { c.Close() })
		tree.capture = c
		tree.intent.Capture = c.Recovery
		meta := scanStatFromInfo(info)
		tree.intent.Identity = captureIdentity{meta.dev, meta.ino}
	}
	tree.mutate = func() error {
		if batch == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if tree.capture != nil {
			captured, e := openRegularAt(parent, path.Base(tree.capture.Recovery), unix.O_RDONLY, 0)
			if e != nil {
				return e
			}
			// Post-capture hashing stays under the serialized commit lock by design.
			// A stat-only check would weaken protection against local writers.
			hash, actual, _, e := stableHash(captured)
			captured.Close()
			if e != nil {
				return e
			}
			if !os.SameFile(info, actual) || hash != l.Hash || (actual.Mode().Perm()&0100 != 0) != l.Exec {
				if batch != nil {
					return errLocalChanged
				}
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
				if batch != nil {
					e = renameNoReplace(parent, path.Base(tree.capture.Recovery), parent, path.Base(batch.conflicts[r.Name]))
				} else {
					_, e = tree.capture.promote(f.me, l.Hash)
				}
				if e != nil {
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
	err = save(tree)
	if batch == nil && err == nil && r.Deleted {
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
	if r.Size < 0 || !validSHA256(r.Hash) {
		return "", errors.New("некорректный размер или SHA-256")
	}
	name := tmpPrefix + captureToken()
	tmp, err := f.root.create(name)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	count := &countingWriter{w: io.MultiWriter(tmp, h), limit: r.Size}
	err = fetch(ctx, r, count)
	if err == nil && count.overflow {
		err = errors.New("получено больше объявленного размера")
	}
	if err == nil && count.n != r.Size {
		err = fmt.Errorf("размер не совпал: получено %d, ожидалось %d", count.n, r.Size)
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(r.Hash) {
		err = fmt.Errorf("хеш не совпал (файл менялся во время передачи)")
	}
	if cerr := closeDownloadedFile(tmp); err == nil {
		err = cerr
	}
	if err != nil {
		_ = f.root.unlink(name, false)
		return "", err
	}
	return name, nil
}

// closeDownloadedFile is an unexported fault-injection hook for close errors.
var closeDownloadedFile = func(file *os.File) error { return file.Close() }

type countingWriter struct {
	w        io.Writer
	limit    int64
	n        int64
	overflow bool
}

func (w *countingWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.n
	if remaining < 0 {
		return 0, errors.New("получено больше объявленного размера")
	}
	if int64(len(p)) > remaining {
		w.overflow = true
		if remaining > 0 {
			n, err := w.w.Write(p[:int(remaining)])
			w.n += int64(n)
			if err != nil {
				return n, err
			}
			if int64(n) != remaining {
				return n, io.ErrShortWrite
			}
		}
		return int(remaining), errors.New("получено больше объявленного размера")
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func validSHA256(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}
