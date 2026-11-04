package folder

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Watch вызывает onChange при любых изменениях в папке. inotify не умеет
// рекурсию, поэтому подписываемся на каждый каталог, включая новые.
func (f *Folder) Watch(ctx context.Context, onChange func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	root := filepath.Clean(f.Root)
	// Watching the parent catches rename/remove/replacement of the root itself;
	// the root watch remains pinned to the original inode.
	if parent := filepath.Dir(root); parent != root {
		if err := w.Add(parent); err != nil {
			f.log.Printf("вотчер: %v", err)
		}
	}
	addTree := func(root string) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			// WalkDir does not follow symlinks, but an event path may name one.
			// Lstat the root and every candidate before subscribing.
			if err != nil || d == nil || !d.IsDir() {
				return nil
			}
			info, err := os.Lstat(p)
			if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if err := w.Add(p); err != nil {
					f.log.Printf("вотчер: %v", err)
				}
			}
			return nil
		})
	}
	addTree(f.Root)
	go func() {
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) {
					if st, err := os.Lstat(ev.Name); err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
						addTree(ev.Name)
					}
				}
				onChange()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				f.log.Printf("вотчер: %v", err)
				notifyWatchError(err, onChange) // overflow просит daemon сделать полный Scan
			}
		}
	}()
	return nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
