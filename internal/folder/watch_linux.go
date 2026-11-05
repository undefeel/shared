package folder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// WatchPaths emits relative dirty scopes. A failed recursive watch is unhealthy:
// emit a full batch and close so the scheduler can switch to polling.
func (f *Folder) WatchPaths(ctx context.Context, emit func(WatchBatch)) (func(), <-chan struct{}, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	gen := f.ConfirmedGeneration()
	root := filepath.Clean(f.Root)
	parent := filepath.Dir(root)
	if parent != root {
		if err = w.Add(parent); err != nil {
			w.Close()
			stop()
			return nil, nil, fmt.Errorf("watch parent: %w", err)
		}
	}
	addTree := func(start string) error { return f.addWatchTree(root, start, w.Add) }
	if err = addTree(root); err != nil {
		w.Close()
		stop()
		return nil, nil, fmt.Errorf("watch tree: %w", err)
	}
	go func() {
		defer close(done)
		defer w.Close()
		full := func(rootChanged bool) {
			emit(watchLossBatch(gen, rootChanged))
		}
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					if watchCtx.Err() == nil {
						full(false)
					}
					return
				}
				if filepath.Clean(ev.Name) == root {
					// chmod/attribute notifications on the watched directory are
					// ordinary metadata changes; only removal or rename means the
					// root binding may have changed.
					if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
						full(true)
					} else {
						full(false)
					}
					continue
				}
				if ev.Has(fsnotify.Create) {
					if st, e := os.Lstat(ev.Name); e == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
						if e = addTree(ev.Name); e != nil {
							full(false)
							return
						}
					}
				}
				rel, ok := f.normalizeWatchPath(ev.Name)
				if !ok {
					continue
				}
				if rel == "" {
					full(false)
					continue
				}
				if ev.Has(fsnotify.Rename) {
					emit(watchRenameBatch(rel, gen))
				} else {
					emit(WatchBatch{Paths: []string{rel}, Generation: gen})
				}
			case e, ok := <-w.Errors:
				if watchCtx.Err() != nil {
					return
				}
				if !ok {
					full(false)
					return
				}
				f.log.Printf("watch: %v", e)
				full(false)
				return
			}
		}
	}()
	return stop, done, nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
