package folder

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsevents"
)

func (f *Folder) WatchPaths(ctx context.Context, emit func(WatchBatch)) (func(), <-chan struct{}, error) {
	watchRoot, err := filepath.EvalSymlinks(f.Root)
	if err != nil {
		return nil, nil, err
	}
	es := &fsevents.EventStream{Paths: []string{watchRoot}, Latency: 300 * time.Millisecond, Flags: fsevents.FileEvents | fsevents.WatchRoot}
	if err := es.Start(); err != nil {
		return nil, nil, err
	}
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	gen := f.ConfirmedGeneration()
	go func() {
		defer close(done)
		defer es.Stop()
		full := func(root bool) { emit(watchLossBatch(gen, root)) }
		for {
			select {
			case <-watchCtx.Done():
				return
			case events, ok := <-es.Events:
				if !ok {
					if watchCtx.Err() == nil {
						full(false)
					}
					return
				}
				for _, e := range events {
					flags := e.Flags
					if darwinWatchLoss(flags) {
						full(false)
						continue
					}
					if flags&fsevents.RootChanged != 0 {
						full(true)
						continue
					}
					rel, inTree, rootEvent := relativeWatchEvent(watchRoot, e.Path)
					if !inTree {
						continue
					}
					if rootEvent {
						// Directory metadata notifications are common during ordinary
						// writes; only the explicit FSEvents RootChanged flag is a
						// root replacement signal.
						continue
					}
					if rel == ignoreFile {
						full(false)
						continue
					}
					if f.Ignored(rel) {
						continue
					}
					if flags&fsevents.ItemRenamed != 0 {
						emit(watchRenameBatch(rel, gen))
					} else {
						emit(WatchBatch{Paths: []string{rel}, Generation: gen})
					}
				}
			}
		}
	}()
	return stop, done, nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }

func darwinWatchLoss(flags fsevents.EventFlags) bool {
	return flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped|fsevents.EventIDsWrapped) != 0
}
