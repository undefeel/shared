package folder

import (
	"errors"
	"os"
)

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
	// Optional per-transaction hooks for cleanup fault injection and tracing.
	discard      func() error
	removeIntent func(string) error
}

// commitLocked is shared by staged Scan and incoming transactions. publish runs
// under mu after the index is durable; cleanup failures cannot undo that commit.
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
	f.mu.Lock()
	publish()
	f.mu.Unlock()
	if tree != nil {
		if cleanupErr := f.cleanupCommit(tree, flushDir); cleanupErr != nil && f.log != nil {
			f.log.Printf("commit cleanup (will retry on Open): %v", cleanupErr)
		}
	}
	return nil
}

func (f *Folder) cleanupCommit(tree *commitTree, flushDir func(*os.File) error) error {
	// Owner policy: capture, then intent, then parent and state flushes.
	if tree.capture != nil {
		discard := tree.discard
		if discard == nil {
			discard = tree.capture.discard
		}
		if err := discard(); err != nil {
			if !errors.Is(err, errCaptureIdentity) {
				return err
			}
			if _, err = tree.capture.promote("recovery", ""); err != nil {
				return err
			}
		}
	}
	remove := tree.removeIntent
	if remove == nil {
		remove = os.Remove
	}
	if err := remove(f.intentPath()); err != nil {
		return err
	}
	var err error
	if tree.capture != nil {
		err = flushDir(tree.capture.parent)
	}
	if err == nil {
		err = f.syncIntentDir()
	}
	if err != nil {
		// Recreate the retry marker if a post-unlink flush failed. Even if
		// its flush fails too, leave the renamed intent available for Open.
		if restoreErr := f.writeIntent(tree.intent); restoreErr != nil && f.log != nil {
			f.log.Printf("restore cleanup intent: %v", restoreErr)
		}
	}
	return err
}
