package folder

import (
	"context"
	"time"

	"github.com/fsnotify/fsevents"
)

// Watch вызывает onChange при любых изменениях в папке. На macOS используется
// FSEvents: один поток на всё дерево, без дескриптора на каждый файл.
func (f *Folder) Watch(ctx context.Context, onChange func()) error {
	es := &fsevents.EventStream{
		Paths:   []string{f.Root},
		Latency: 300 * time.Millisecond,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := es.Start(); err != nil {
		return err
	}
	go func() {
		defer es.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-es.Events:
				onChange()
			}
		}
	}()
	return nil
}
