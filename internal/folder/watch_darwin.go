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
			case events, ok := <-es.Events:
				if !ok {
					return
				}
				// FSEvents marks dropped history with MustScanSubDirs, and root
				// replacement with RootChanged. Every batch requests a full Scan,
				// so these conditions need no separate recovery path.
				if len(events) != 0 {
					onChange()
				}
			}
		}
	}()
	return nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
