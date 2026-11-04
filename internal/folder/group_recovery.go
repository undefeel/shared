package folder

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"reflect"
	"sort"

	"golang.org/x/sys/unix"
)

func recoveryFile(fi *FileInfo) *storeFile {
	if fi == nil {
		return nil
	}
	s := &storeFile{Name: fi.Name, Disk: fi.Disk, Hash: fi.Hash, Size: fi.Size, ModTime: fi.ModTime, Exec: fi.Exec, Deleted: fi.Deleted, DeletedAt: fi.StoreDeletedAt}
	for peer, counter := range fi.Version {
		s.Version = append(s.Version, storePair{peer, counter})
	}
	sort.Slice(s.Version, func(i, j int) bool { return s.Version[i].Peer < s.Version[j].Peer })
	return s
}

// recoverGroup runs exclusively at startup, before Scan/watcher/network service.
// Call without locks: hashing runs outside scanMu/mu.
// state and commit must come from verified replay; nil commit means no complete
// frame for this TxID. No index publication or per-entry roll-forward occurs here.
func (f *Folder) recoverGroup(g groupIntent, state storeSnapshot, commit *storeFrame) error {
	ambiguous := func() error {
		return &RootError{Code: "recovery_ambiguous", Message: "group recovery evidence differs", Token: captureToken()}
	}
	if err := g.validate(f.binding, f.confirmedGeneration); err != nil {
		return err
	}
	f.scanMu.Lock()
	err := f.guardRoot()
	f.scanMu.Unlock()
	if err != nil {
		return err
	}
	if state.StoreID != g.StoreID || state.Control.Binding != g.Binding || state.Control.ConfirmedGeneration != g.ConfirmedGeneration {
		return ambiguous()
	}
	committed := commit != nil
	expected := g.BaseGeneration
	if committed {
		expected = g.Generation
		if commit.validate() != nil || commit.Kind != "Commit" || commit.TxID != g.TxID || commit.PreviousGeneration != g.BaseGeneration || commit.Generation != g.Generation || commit.Clock != g.Clock || len(commit.Operations) != len(g.Entries) {
			return ambiguous()
		}
	}
	if state.Generation != expected || (committed && state.Clock != g.Clock) {
		return ambiguous()
	}
	for _, e := range g.Entries {
		name := recordName(journalIntent{Before: e.Before, After: e.After})
		var current *storeFile
		for i := range state.Files {
			if state.Files[i].Name == name {
				current = &state.Files[i]
				break
			}
		}
		want := recoveryFile(e.Before)
		if committed {
			want = recoveryFile(e.After)
		}
		if !reflect.DeepEqual(current, want) {
			return ambiguous()
		}
		if committed {
			found := false
			for _, op := range commit.Operations {
				if op.Name == name {
					var digest [32]byte
					if b := recoveryFile(e.Before); b != nil {
						digest = storeDigest(*b)
					}
					found = op.Before == digest && reflect.DeepEqual(op.After, want) && op.DeleteRecord == (want == nil)
				}
			}
			if !found {
				return ambiguous()
			}
			if e.After != nil && !e.After.Deleted {
				if err := f.checkRecoveryFile(e.Target, e.After, nil); err != nil {
					return err
				}
			}
		}
	}
	flush := f.persistHooks.syncDir
	if flush == nil {
		flush = syncDir
	}
	for i := range g.Entries {
		e := &g.Entries[i]
		if e.Capture == "" {
			continue
		} // New targets remain for the subsequent full Scan.
		c, err := f.root.openCapture(e.Target, e.Capture, e.Identity)
		if err != nil {
			return err
		}
		err = func() error {
			defer c.Close()
			file, err := f.root.openRead(e.Capture)
			if errors.Is(err, os.ErrNotExist) {
				if committed {
					return flush(c.parent)
				}
				if err = f.checkRecoveryFile(e.Target, e.Before, &e.Identity); err != nil {
					var rootErr *RootError
					if !errors.Is(err, os.ErrNotExist) && !errors.As(err, &rootErr) {
						return err
					}
					if e.Conflict == "" {
						return ambiguous()
					}
					if err = f.checkRecoveryFile(e.Conflict, e.Before, &e.Identity); err != nil {
						return err
					}
				}
				return flush(c.parent)
			}
			if err != nil {
				return err
			}
			file.Close()
			if committed {
				err = c.discard()
				if err == nil {
					return flush(c.parent)
				}
				if !errors.Is(err, errCaptureIdentity) {
					return err
				}
			} else if err = f.checkRecoveryFile(e.Capture, e.Before, &e.Identity); err != nil {
				return err
			}
			if !committed {
				err = c.ops.renameNoReplace(c.parent, path.Base(e.Capture), c.parent, path.Base(e.Target))
				if err == nil {
					return flush(c.parent)
				}
				if !errors.Is(err, unix.EEXIST) {
					return err
				}
			}
			for {
				if e.Conflict == "" {
					e.Conflict = path.Join(path.Dir(e.Target), conflictLeaf(e.Target, "recovery", "", captureToken()))
					if err = f.writeGroupRecovery(g); err != nil {
						return err
					}
				}
				err = c.ops.renameNoReplace(c.parent, path.Base(e.Capture), c.parent, path.Base(e.Conflict))
				if errors.Is(err, unix.EEXIST) {
					e.Conflict = ""
					continue
				}
				if err != nil {
					return err
				}
				return flush(c.parent)
			}
		}()
		if err != nil {
			return err
		}
	}
	// Flushes above precede marker removal, making rollback repeatable even after
	// an interrupted directory flush. Keep the marker on cleanup durability error.
	if err := os.Remove(f.intentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := f.syncIntentDir(); err != nil {
		_ = f.writeGroupRecovery(g)
		return err
	}
	return nil
}

func (f *Folder) checkRecoveryFile(name string, want *FileInfo, identity *captureIdentity) error {
	file, err := f.root.openRead(name)
	if err != nil {
		return err
	}
	defer file.Close()
	hash, info, _, err := stableHash(file)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	if want == nil || hash != want.Hash || info.Size() != want.Size || (info.Mode().Perm()&0111 != 0) != want.Exec || (identity != nil && *identity != (captureIdentity{uint64(st.Dev), uint64(st.Ino)})) {
		return &RootError{Code: "recovery_ambiguous", Message: "recovery file differs", Token: captureToken()}
	}
	return nil
}

// Persist a changed conflict reservation before promotion; never overwrite data.
func (f *Folder) writeGroupRecovery(g groupIntent) error {
	if err := g.validate(f.binding, f.confirmedGeneration); err != nil {
		return err
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(path.Dir(f.intentPath()), ".sharedir-intent-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := tmp.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	flush := f.persistHooks.syncFile
	if flush == nil {
		flush = syncFile
	}
	if err = flush(tmp); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), f.intentPath()); err != nil {
		return err
	}
	return f.syncIntentDir()
}
