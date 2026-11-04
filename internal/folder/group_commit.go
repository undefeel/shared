package folder

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
)

// commitGroupLocked consumes already checked, pinned preparations. The caller
// holds scanMu -> saveMu, rechecks decisions/binding/cancellation before entry,
// and does all hashing outside these locks. No cancellation is checked after G2.
// preparedDirs is leaf-first and includes all directories created in staging.
func (f *Folder) commitGroupLocked(g groupIntent, trees []*commitTree, preparedDirs []*os.File, publish func(), beforeIntent ...func() error) (err error) {
	if err = g.validate(f.binding, f.confirmedGeneration); err != nil {
		return err
	}
	if f.store == nil || len(trees) != len(g.Entries) || g.StoreID != f.store.state.StoreID || g.BaseGeneration != f.store.state.Generation {
		return fmt.Errorf("stale group preparation")
	}
	f.mu.Lock()
	blocked := f.blocked
	f.mu.Unlock()
	if blocked != nil {
		return blocked
	}
	if err = f.guardRoot(); err != nil {
		return err
	}
	// Never overwrite a previous group's cleanup marker. Only a commit verified
	// in this process may be cleaned here; restart recovery handles uncertainty.
	if b, e := os.ReadFile(f.intentPath()); e == nil {
		var old groupIntent
		if e = json.Unmarshal(b, &old); e != nil || old.validate(f.binding, f.confirmedGeneration) != nil || old.StoreID != g.StoreID || old.Generation != f.store.state.Generation {
			return fmt.Errorf("pending intent requires recovery")
		}
		if e = f.cleanupGroup(&old); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// The public FileInfo deliberately omits store-only tombstone metadata.
	// Put the exact durable fields into private intent copies before encoding.
	oldFiles := make(map[string]storeFile, len(f.store.state.Files))
	for _, fi := range f.store.state.Files {
		oldFiles[fi.Name] = fi
	}
	for i := range g.Entries {
		e := &g.Entries[i]
		if e.Before != nil {
			c := *e.Before
			c.StoreDeletedAt = oldFiles[c.Name].DeletedAt
			e.Before = &c
		}
		if e.After != nil {
			c := *e.After
			if c.Deleted {
				c.StoreDeletedAt = oldFiles[c.Name].DeletedAt
				if c.StoreDeletedAt == 0 {
					c.StoreDeletedAt = f.persistHooks.clockNow().UnixNano()
				}
			}
			e.After = &c
		}
	}
	frame := storeFrame{Kind: "Commit", TxID: g.TxID, PreviousGeneration: g.BaseGeneration, Generation: g.Generation, Clock: g.Clock}
	for i, e := range g.Entries {
		if trees[i] == nil || trees[i].mutate == nil {
			return fmt.Errorf("missing group preparation")
		}
		op := storeOperation{Name: recordName(journalIntent{Before: e.Before, After: e.After}), After: recoveryFile(e.After), DeleteRecord: e.After == nil}
		if e.Before != nil {
			op.Before = storeDigest(*recoveryFile(e.Before))
		}
		frame.Operations = append(frame.Operations, op)
	}
	sort.Slice(frame.Operations, func(i, j int) bool { return frame.Operations[i].Name < frame.Operations[j].Name })
	if _, err = applyStoreFrame(f.store.state, frame); err != nil {
		return err
	}
	if _, err = encodeFrame(frame); err != nil {
		return err
	} // Size limit before intent.
	intentStarted := false
	defer func() {
		if err != nil && (intentStarted || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && err != errLocalChanged)) {
			f.mu.Lock()
			if f.blocked == nil {
				f.blocked = &RootError{Code: "commit_failed", Message: err.Error(), Token: captureToken()}
			}
			f.mu.Unlock()
		}
	}()
	tx := hex.EncodeToString(g.TxID[:])
	f.persistHooks.phase(tx, "G1")
	var devices []*os.File
	for _, t := range trees {
		if t.incoming != nil {
			if err = f.persistHooks.fsyncOnly(t.incoming, "G1"); err != nil {
				return err
			}
			devices = append(devices, t.incoming)
		}
	}
	if err = f.flushGroupDirs(preparedDirs, "G1"); err != nil {
		return err
	}
	devices = append(devices, preparedDirs...)
	if err = f.persistHooks.fullSyncDevices(devices, "G1"); err != nil {
		return err
	}
	for _, check := range beforeIntent {
		if err = check(); err != nil {
			return err
		}
	}
	intentStarted = true
	f.persistHooks.phase(tx, "G2")
	if err = f.writeGroupIntent(g); err != nil {
		return err
	}
	f.persistHooks.phase(tx, "G3")
	var dirs []*os.File
	for _, t := range trees {
		if t.capture != nil {
			if err = t.capture.take(); err != nil {
				return err
			}
			dirs = append(dirs, t.capture.parent)
		}
	}
	if err = f.flushGroupDirs(dirs, "G3a"); err != nil {
		return err
	}
	for _, t := range trees {
		if err = t.mutate(); err != nil {
			return err
		}
		dirs = append(dirs, t.dirs...)
	}
	if err = f.flushGroupDirs(dirs, "G3b"); err != nil {
		return err
	}
	f.persistHooks.phase(tx, "G4")
	if err = f.store.append(frame); err != nil {
		return err
	}
	f.durableGeneration = g.Generation
	f.mu.Lock()
	f.generation = g.Generation
	publish()
	f.mu.Unlock()
	f.persistHooks.phase(tx, "G5")
	if e := f.cleanupGroup(&g); e != nil && f.log != nil {
		f.log.Printf("group cleanup (will retry): %v", e)
	}
	return nil
}

func (f *Folder) flushGroupDirs(dirs []*os.File, phase string) error {
	seen := make(map[[2]uint64]bool)
	for _, d := range dirs {
		dev, ino, err := fileDevice(d)
		if err != nil {
			return err
		}
		key := [2]uint64{dev, ino}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err = f.persistHooks.fsyncOnly(d, phase); err != nil {
			return err
		}
	}
	return nil
}

func (f *Folder) writeGroupIntent(g groupIntent) error {
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
	n, err := f.persistHooks.write("intent", tmp.Name(), tmp, b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	if err = f.persistHooks.durableFile(tmp, "G2"); err != nil {
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

func (f *Folder) cleanupGroup(g *groupIntent) error {
	var parents []*os.File
	defer func() {
		for _, p := range parents {
			p.Close()
		}
	}()
	for i := range g.Entries {
		e := &g.Entries[i]
		if e.Capture == "" {
			continue
		}
		c, err := f.root.openCapture(e.Target, e.Capture, e.Identity)
		if err != nil {
			return err
		}
		parents = append(parents, c.parent)
		err = c.discard()
		if errors.Is(err, errCaptureIdentity) {
			// Reserve the destination durably before promotion, including collisions.
			for {
				if e.Conflict == "" {
					e.Conflict = path.Join(path.Dir(e.Target), conflictLeaf(e.Target, "recovery", "", captureToken()))
					if err = f.writeGroupIntent(*g); err != nil {
						return err
					}
				}
				err = c.ops.renameNoReplace(c.parent, path.Base(e.Capture), c.parent, path.Base(e.Conflict))
				if errors.Is(err, os.ErrExist) {
					e.Conflict = ""
					continue
				}
				break
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(f.intentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	err := f.flushGroupDirs(parents, "G5")
	if err == nil {
		err = f.syncIntentDir()
	}
	if err != nil {
		_ = f.writeGroupIntent(*g)
	}
	return err
}
