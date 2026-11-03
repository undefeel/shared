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
	addTree := func(root string) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
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
			case ev := <-w.Events:
				if ev.Has(fsnotify.Create) {
					if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
						addTree(ev.Name)
					}
				}
				onChange()
			case err := <-w.Errors:
				f.log.Printf("вотчер: %v", err)
				onChange() // переполнение очереди и т.п. — просто пересканируем
			}
		}
	}()
	return nil
}
