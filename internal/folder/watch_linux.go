package folder

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	addTree := func(start string) error {
		return filepath.WalkDir(start, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d == nil {
				return fmt.Errorf("watch: missing entry %s", p)
			}
			rel, _ := filepath.Rel(root, p)
			if rel != "." && f.Ignored(strings.ReplaceAll(rel, string(filepath.Separator), "/")) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			st, e := os.Lstat(p)
			if e != nil {
				return e
			}
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			return w.Add(p)
		})
	}
	if err = addTree(root); err != nil {
		w.Close()
		stop()
		return nil, nil, fmt.Errorf("watch tree: %w", err)
	}
	go func() {
		defer close(done)
		defer w.Close()
		full := func(rootChanged bool) {
			emit(WatchBatch{FullRequired: true, RootChanged: rootChanged, Generation: gen})
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
				paths := []string{rel}
				renamed := []string(nil)
				renameFull := false
				if ev.Has(fsnotify.Rename) {
					renamed = append(renamed, rel)
					renameFull = true // fsnotify does not pair old and new names.
					p := filepath.ToSlash(filepath.Dir(rel))
					if p == "." {
						p = ""
					}
					if p != "" {
						paths = append(paths, p)
					}
				}
				emit(WatchBatch{Paths: paths, RenamePaths: renamed, RenameFull: renameFull, FullRequired: renameFull, Generation: gen})
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
