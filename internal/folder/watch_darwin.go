package folder

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsevents"
)

func (f *Folder) WatchPaths(ctx context.Context, emit func(WatchBatch)) (func(), <-chan struct{}, error) {
	es := &fsevents.EventStream{Paths: []string{f.Root}, Latency: 300 * time.Millisecond, Flags: fsevents.FileEvents | fsevents.WatchRoot}
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
					rel, err := filepath.Rel(f.Root, e.Path)
					if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
						full(true)
						continue
					}
					rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
					if rel == ignoreFile {
						full(false)
						continue
					}
					if f.Ignored(rel) {
						continue
					}
					paths := []string{rel}
					if flags&fsevents.ItemRenamed != 0 {
						p := filepath.ToSlash(filepath.Dir(rel))
						if p == "." {
							full(false)
							continue
						}
						paths = append(paths, p)
					}
					emit(WatchBatch{Paths: paths, Generation: gen})
				}
			}
		}
	}()
	return stop, done, nil
}

func notifyWatchError(_ error, onChange func()) { onChange() }
