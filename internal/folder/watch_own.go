package folder

import (
	"context"
)

const ownEventLimit = 4096

type ownFingerprint struct {
	generation uint64
	stat       scanStat
	hash       string
	info       FileInfo
}

// registerOwnEvents runs only after G4 publication. Unknown or unverifiable
// targets are deliberately omitted so their watcher events remain dirty.
func (f *Folder) registerOwnEvents(g groupIntent) {
	lease := f.root
	if lease == nil || !lease.acquireLease() {
		return
	}
	defer lease.releaseLease()
	items := make(map[string]ownFingerprint)
	for _, e := range g.Entries {
		if e.After == nil || e.After.Deleted || e.After.Hash == "" {
			continue
		}
		name := e.Target
		if name == "" || name == "." {
			continue
		}
		meta, err := scanStatAt(lease, name)
		if err != nil || meta.dev == 0 || meta.ino == 0 || meta.mode&uint32(0xf000) != 0x8000 || meta.size != e.After.Size {
			continue
		}
		info := *e.After
		info.Version = e.After.Version.Copy()
		items[name] = ownFingerprint{generation: g.ConfirmedGeneration, stat: meta, hash: e.After.Hash, info: info}
	}
	f.ownMu.Lock()
	if f.ownEvents == nil {
		f.ownEvents = make(map[string]ownFingerprint)
	}
	for name, fp := range items {
		f.ownEvents[name] = fp
	}
	for len(f.ownEvents) > ownEventLimit {
		for name := range f.ownEvents {
			delete(f.ownEvents, name)
			break
		}
	}
	f.ownMu.Unlock()
}

// FilterWatchBatch removes only fully verified incoming writes. Any uncertainty,
// mixed path or generation change preserves the event for incremental scanning.
func (f *Folder) FilterWatchBatch(ctx context.Context, b WatchBatch) WatchBatch {
	if b.RootChanged || (b.FullRequired && !b.RenameFull) || (len(b.Paths) == 0 && len(b.RenamePaths) == 0) {
		return b
	}
	wasRenameFull := b.RenameFull
	// Pin the root while scanMu prevents rebind; drop both locks before any
	// rooted filesystem work or hashing.
	f.scanMu.Lock()
	f.mu.Lock()
	generation, root := f.confirmedGeneration, f.root
	if generation != b.Generation || root == nil || !root.acquireLease() {
		f.mu.Unlock()
		f.scanMu.Unlock()
		return b
	}
	f.mu.Unlock()
	f.scanMu.Unlock()
	defer root.releaseLease()
	out := make([]string, 0, len(b.Paths))
	keptRenames := make([]string, 0, len(b.RenamePaths))
	verify := func(name string) bool {
		if ctx.Err() != nil {
			return false
		}
		if name == "" || name == "." {
			return false
		}
		f.ownMu.Lock()
		fp, ok := f.ownEvents[name]
		f.ownMu.Unlock()
		if !ok || fp.generation != generation {
			return false
		}
		before, err := scanStatAt(root, name)
		if err != nil || before != fp.stat || before.mode&uint32(0xf000) != 0x8000 {
			return false
		}
		file, err := f.rootScanIO().openRead(root, name)
		if err != nil {
			return false
		}
		hash, _, after, hashErr := stableHash(file)
		file.Close()
		if hashErr != nil || after != fp.stat || hash != fp.hash {
			return false
		}
		// The hashed descriptor must still name the event target.
		if current, err := scanStatAt(root, name); err != nil || current != fp.stat {
			return false
		}
		f.saveMu.Lock()
		f.mu.Lock()
		cur := f.files[fp.info.Name]
		matches := f.confirmedGeneration == generation && cur != nil && !cur.Deleted && cur.Hash == fp.info.Hash && cur.Size == fp.info.Size && cur.ModTime == fp.info.ModTime && cur.Exec == fp.info.Exec && cur.Version.Compare(fp.info.Version) == Equal
		f.mu.Unlock()
		f.saveMu.Unlock()
		return matches
	}
	for _, name := range b.Paths {
		if !verify(name) {
			out = append(out, name)
		}
	}
	for _, name := range b.RenamePaths {
		if !verify(name) {
			keptRenames = append(keptRenames, name)
		}
	}
	b.Paths = out
	b.RenamePaths = keptRenames
	b.RenameFull = wasRenameFull && len(keptRenames) > 0
	b.FullRequired = b.FullRequired && (!wasRenameFull || len(keptRenames) > 0)
	return b
}
