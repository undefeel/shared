package folder

import "os"

// commitTree describes prepared data. mutate must use pinned parents and
// no-replace operations, recheck captured bytes, and leave the intent on error.
// dirs includes every changed directory, including parents created during staging.
// The caller holds scanMu and saveMu (in that order), never mu during I/O.
type commitTree struct {
	intent   journalIntent
	incoming *os.File
	capture  *capture
	dirs     []*os.File
	mutate   func() error
}

// commitLocked is shared by staged Scan and incoming transactions. publish runs
// under mu only after all durable operations succeed; callers report Done after return.
func (f *Folder) commitLocked(p persisted, tree *commitTree, publish func()) (err error) {
	defer func() {
		if err != nil {
			// Never overwrite an uncertain index or intent with a second save.
			// Recovery owns the on-disk transaction; stop this process meanwhile.
			f.mu.Lock()
			if f.blocked == nil {
				f.blocked = &RootError{Code: "commit_failed", Message: err.Error(), Token: captureToken()}
			}
			f.mu.Unlock()
		}
	}()
	flushFile, flushDir := f.persistHooks.syncFile, f.persistHooks.syncDir
	if flushFile == nil {
		flushFile = syncFile
	}
	if flushDir == nil {
		flushDir = syncDir
	}
	if tree != nil {
		if tree.incoming != nil {
			if err := flushFile(tree.incoming); err != nil {
				return err
			}
		}
		tree.intent.Generation = p.Generation
		if err := f.writeIntent(tree.intent); err != nil {
			return err
		}
		if tree.capture != nil {
			if err := tree.capture.take(); err != nil {
				return err
			}
			if err := flushDir(tree.capture.parent); err != nil {
				return err
			}
		}
		if err := tree.mutate(); err != nil {
			return err
		}
		for _, d := range tree.dirs {
			if err := flushDir(d); err != nil {
				return err
			}
		}
	}
	f.generation = p.Generation
	if err := f.persistLocked(p); err != nil {
		return err
	}
	if tree != nil {
		// Owner policy: capture, then intent, then parent and state flushes.
		if tree.capture != nil {
			if err := tree.capture.discard(); err != nil {
				return err
			}
		}
		if err := os.Remove(f.intentPath()); err != nil {
			return err
		}
		if tree.capture != nil {
			if err := flushDir(tree.capture.parent); err != nil {
				return err
			}
		}
		if err := f.syncIntentDir(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	publish()
	f.mu.Unlock()
	return nil
}
