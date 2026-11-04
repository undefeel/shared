package folder

import (
	"context"
	"time"

	"github.com/fsnotify/fsevents"
)

// Watch вызывает onChange при любых изменениях в папке. stop отменяет
// подписку, done закрывается после Stop для нативного FSEvents stream.
// На macOS FSEvents использует один поток на всё дерево.
func (f *Folder) Watch(ctx context.Context, onChange func()) (func(), <-chan struct{}, error) {
	es := &fsevents.EventStream{
		Paths:   []string{f.Root},
		Latency: 300 * time.Millisecond,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := es.Start(); err != nil {
		return nil, nil, err
	}
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer es.Stop()
		for {
			select {
			case <-watchCtx.Done():
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
	return stop, done, nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
