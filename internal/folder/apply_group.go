package folder

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
)

type applyOperation struct {
	peer   string
	step   step
	staged *stagedApply
}
type applyBatch struct {
	next      map[string]*FileInfo
	clock     uint64
	trees     []*commitTree
	cleanup   []func()
	conflicts map[string]string
}

func copyApplyFiles(files map[string]*FileInfo) map[string]*FileInfo {
	next := make(map[string]*FileInfo, len(files))
	for name, fi := range files {
		c := *fi
		c.Version = fi.Version.Copy()
		next[name] = &c
	}
	return next
}

func (f *Folder) submitApply(ctx context.Context, peer string, generation uint64, st step, staged *stagedApply) (step, error) {
	op := &applyOperation{peer, st, staged}
	f.applyMu.Lock()
	if f.applyCoordinator == nil {
		f.applyCoordinator = newGroupCoordinator(f.applyGroup)
	}
	c := f.applyCoordinator
	f.applyMu.Unlock()
	size := int64(0)
	if st.act != mergeOnly && st.act != skip && !st.r.Deleted {
		size = st.r.Size
	}
	err := c.submit(ctx, []string{st.r.Name}, generation, size, op)
	return op.step, err
}

// drainApply is called before releasing a root or binding. Callers already
// cancel/drain peer runners before confirmation; never wait with scanMu held.
func (f *Folder) drainApply() {
	f.applyMu.Lock()
	c := f.applyCoordinator
	if c != nil {
		c.close(true)
		f.applyCoordinator = nil
	}
	f.applyMu.Unlock()
}

func (f *Folder) applyGroup(requests []*groupRequest) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	batch := &applyBatch{next: copyApplyFiles(f.files), clock: f.clock, conflicts: make(map[string]string)}
	before := copyApplyFiles(f.files)
	f.mu.Unlock()
	defer func() {
		for i := len(batch.cleanup) - 1; i >= 0; i-- {
			batch.cleanup[i]()
		}
	}()
	for _, req := range requests {
		op := req.apply
		final, err := f.prepareApply(req.Context, op.peer, req.Generation, op.step, op.staged, batch)
		if err != nil {
			return err
		}
		op.step = final
	}
	// Cancellation and epoch are checked once more immediately before intent.
	for _, req := range requests {
		if err := req.Context.Err(); err != nil {
			return err
		}
		if req.Generation != f.ConfirmedGeneration() {
			return errLocalChanged
		}
	}
	g := groupIntent{Schema: storeSchema, StoreID: f.store.state.StoreID, BaseGeneration: f.store.state.Generation, Generation: f.store.state.Generation + 1, ConfirmedGeneration: f.confirmedGeneration, Binding: f.binding, Clock: batch.clock}
	if _, err := rand.Read(g.TxID[:]); err != nil {
		return fmt.Errorf("group tx id: %w", err)
	}
	var trees []*commitTree
	var dirs []*os.File
	for _, req := range requests {
		name := req.apply.step.r.Name
		after := batch.next[name]
		old := before[name]
		if old != nil && after != nil && storeDigest(*recoveryFile(old)) == storeDigest(*recoveryFile(after)) {
			continue
		}
		if old == nil && after == nil {
			continue
		}
		var tree *commitTree
		for _, t := range batch.trees {
			if t.intent.After.Name == name {
				tree = t
				break
			}
		}
		entry := groupIntentEntry{Target: req.apply.staged.disk, Before: old, After: after}
		if tree == nil {
			tree = &commitTree{mutate: func() error { return nil }}
		} else {
			// A newly discovered local version must be scanned/replanned before
			// capturing it: recovery Before must describe both index and old bytes.
			if tree.intent.Before != nil && old != nil && storeDigest(*recoveryFile(tree.intent.Before)) != storeDigest(*recoveryFile(old)) {
				return errLocalChanged
			}
			entry.Tmp, entry.Capture, entry.Identity = tree.intent.Tmp, tree.intent.Capture, tree.intent.Identity
			if tree.incoming != nil {
				dev, ino, err := fileDevice(tree.incoming)
				if err != nil {
					return err
				}
				entry.IncomingIdentity = captureIdentity{dev, ino}
			}
			// Conflict promotion must use the name reserved by the durable intent.
			if req.apply.step.act == takeWithConflict {
				entry.Conflict = path.Join(path.Dir(entry.Target), conflictLeaf(entry.Target, f.me, old.Hash, captureToken()))
				batch.conflicts[name] = entry.Conflict
			}
			dirs = append(dirs, tree.dirs...)
		}
		g.Entries = append(g.Entries, entry)
		trees = append(trees, tree)
	}
	if len(g.Entries) == 0 {
		return nil
	}
	if err := f.commitGroupLocked(g, trees, dirs, func() { f.files = batch.next; f.clock = batch.clock; f.notify() }, func() error {
		for _, req := range requests {
			if err := req.Context.Err(); err != nil {
				return err
			}
			if req.Generation != f.ConfirmedGeneration() {
				return errLocalChanged
			}
		}
		return f.guardRootWithSaveLock(true)
	}); err != nil {
		return err
	}
	// Prune empty parents only after captures and the durable group are settled.
	for _, e := range g.Entries {
		if e.After == nil || !e.After.Deleted {
			continue
		}
		for dir := path.Dir(e.Target); dir != "."; dir = path.Dir(dir) {
			if err := f.root.unlink(dir, true); err != nil {
				break
			}
			parent, _, err := f.root.parent(dir, false)
			if err != nil {
				return err
			}
			err = f.persistHooks.fsyncOnly(parent, "G5")
			parent.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Open has already verified/truncated the log before dispatching recovery.
func (f *Folder) recoverApplyIntent() error {
	data, err := os.ReadFile(f.intentPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct{ Schema uint32 }
	if err = json.Unmarshal(data, &header); err != nil {
		return err
	}
	if header.Schema == 0 {
		return f.recoverIntent()
	}
	var g groupIntent
	if err = json.Unmarshal(data, &g); err != nil {
		return err
	}
	log, err := os.Open(f.store.log.Name())
	if err != nil {
		return err
	}
	defer log.Close()
	if _, err = log.Seek(logHeaderSize, io.SeekStart); err != nil {
		return err
	}
	var committed *storeFrame
	for {
		frame, e := decodeFrame(log)
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if frame.TxID == g.TxID {
			c := frame
			committed = &c
		}
	}
	return f.recoverGroup(g, f.store.state, committed)
}
