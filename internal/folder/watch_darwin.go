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
		full := func(root bool) { emit(WatchBatch{FullRequired: true, RootChanged: root, Generation: gen}) }
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
					if flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped|fsevents.EventIDsWrapped) != 0 {
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
					paths := []string{rel}
					renamed := []string(nil)
					renameFull := false
					if flags&fsevents.ItemRenamed != 0 {
						renamed = append(renamed, rel)
						p := filepath.ToSlash(filepath.Dir(rel))
						if p != "." {
							paths = append(paths, p)
						} else {
							renameFull = true
						}
					}
					emit(WatchBatch{Paths: paths, RenamePaths: renamed, RenameFull: renameFull, FullRequired: renameFull, Generation: gen})
				}
			}
		}
	}()
	return stop, done, nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
