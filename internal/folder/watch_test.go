package folder

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func watchFixture(t *testing.T) *Folder {
	t.Helper()
	dir := t.TempDir()
	f, err := Open(filepath.Join(dir, "root"), filepath.Join(dir, "index.json"), "watch", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func waitWatch(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("watch event timeout")
	}
}

func startTestWatch(t *testing.T, f *Folder) (func(), <-chan struct{}, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan struct{}, 32)
	stop, done, err := f.Watch(ctx, func() {
		select {
		case events <- struct{}{}:
		default:
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); <-done; cancel() })
	return stop, done, events
}

func TestWatchRootReplacementSignalsChange(t *testing.T) {
	f := watchFixture(t)
	_, _, events := startTestWatch(t, f)
	old := f.Root + ".old"
	if err := os.Rename(f.Root, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.Root, 0755); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, events)
	if err := f.Scan(); err == nil || f.Blocked() == nil || f.Blocked().Code != "root_changed" {
		t.Fatalf("replacement Scan error=%v blocked=%+v, want root_changed", err, f.Blocked())
	}
}

func TestWatchEventDuringScanIsRetainedForFullScan(t *testing.T) {
	f := watchFixture(t)
	_, _, events := startTestWatch(t, f)
	f.scanMu.Lock()
	if err := os.WriteFile(filepath.Join(f.Root, "during-scan"), []byte("x"), 0644); err != nil {
		f.scanMu.Unlock()
		t.Fatal(err)
	}
	waitWatch(t, events)
	f.scanMu.Unlock()
	if err := f.ScanGeneration(context.Background(), f.ConfirmedGeneration()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.Lookup("during-scan"); !ok {
		t.Fatal("full Scan missed event that arrived during scan")
	}
}

func TestWatchRebindUsesNewRootSubscription(t *testing.T) {
	f := watchFixture(t)
	oldRoot := f.Root
	oldStop, oldDone, oldEvents := startTestWatch(t, f)
	if err := f.blockRoot("root_changed", "test rebind"); err == nil {
		t.Fatal("expected blocked root")
	}
	blocked := f.Blocked()
	newRoot := f.Root + ".new"
	if err := os.Mkdir(newRoot, 0755); err != nil {
		t.Fatal(err)
	}
	oldStop()
	<-oldDone
	for {
		select {
		case <-oldEvents:
		default:
			goto oldDrained
		}
	}
oldDrained:
	if err := f.Confirm(blocked.Token, "rebind", newRoot); err != nil {
		t.Fatal(err)
	}
	_, _, events := startTestWatch(t, f)
	if err := os.WriteFile(filepath.Join(oldRoot, "old-tree-write"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
		t.Fatal("new watcher observed an old-tree event after rebind")
	case <-oldEvents:
		t.Fatal("closed watcher delivered an old-tree event after rebind")
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(newRoot, "new-file"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, events)
}

func TestWatchDoesNotObserveExternalTreeThroughSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux addTree does not follow directory symlinks")
	}
	f := watchFixture(t)
	external := t.TempDir()
	link := filepath.Join(f.Root, "outside")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	_, _, events := startTestWatch(t, f)
	// Drain the event for creating the link before writing in the target.
	time.Sleep(100 * time.Millisecond)
	for {
		select {
		case <-events:
		default:
			goto drained
		}
	}
drained:
	if err := os.WriteFile(filepath.Join(external, "secret"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
		t.Fatal("external tree generated a watcher event")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchOverflowRequestsFullScan(t *testing.T) {
	f := watchFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, "overflow-recovery"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	notifyWatchError(errors.New("synthetic fsnotify overflow"), func() { called <- f.ScanGeneration(context.Background(), f.ConfirmedGeneration()) })
	if err := <-called; err != nil {
		t.Fatalf("overflow full Scan: %v", err)
	}
	if _, _, ok := f.Lookup("overflow-recovery"); !ok {
		t.Fatal("overflow did not result in a full Scan")
	}
}

func TestScanWatchBatchFencesGenerationAndEmptyPaths(t *testing.T) {
	f := watchFixture(t)
	gen := f.ConfirmedGeneration()
	if err := f.ScanWatchBatch(context.Background(), WatchBatch{Generation: gen}); err != nil {
		t.Fatalf("empty watcher batch should be a no-op: %v", err)
	}
	f.scanMu.Lock()
	f.mu.Lock()
	f.confirmedGeneration++
	f.mu.Unlock()
	f.scanMu.Unlock()
	if err := f.ScanWatchBatch(context.Background(), WatchBatch{Paths: []string{"changed"}, Generation: gen}); !errors.Is(err, ErrStaleWatchGeneration) {
		t.Fatalf("stale batch error = %v", err)
	}
}

func TestRelativeWatchEventScopesRootAndOutsidePaths(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "private", "var", "sync")
	if rel, inTree, rootEvent := relativeWatchEvent(root, root); rel != "" || !inTree || !rootEvent {
		t.Fatalf("root event = (%q,%v,%v)", rel, inTree, rootEvent)
	}
	if rel, inTree, rootEvent := relativeWatchEvent(root, filepath.Join(root, "dir", "file")); rel != "dir/file" || !inTree || rootEvent {
		t.Fatalf("file event = (%q,%v,%v)", rel, inTree, rootEvent)
	}
	if rel, inTree, rootEvent := relativeWatchEvent(root, filepath.Join(filepath.Dir(root), "other", "file")); rel != "" || inTree || rootEvent {
		t.Fatalf("outside event = (%q,%v,%v)", rel, inTree, rootEvent)
	}
}

func TestWatchTreeInstallsParentFirstAndPropagatesErrors(t *testing.T) {
	f := watchFixture(t)
	child := filepath.Join(f.Root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	var installed []string
	sentinel := errors.New("watch Add failed")
	err := f.addWatchTree(f.Root, f.Root, func(p string) error {
		installed = append(installed, p)
		if p == child {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) || len(installed) != 2 || installed[0] != f.Root {
		t.Fatalf("order=%v err=%v", installed, err)
	}
	if err := f.addWatchTree(f.Root, filepath.Join(f.Root, "missing"), func(string) error { return nil }); err == nil {
		t.Fatal("walk error swallowed")
	}
}

func TestWatchLossAndRenameScopes(t *testing.T) {
	for _, cause := range []string{"overflow", "dropped", "addTree", "stream close"} {
		t.Run(cause, func(t *testing.T) {
			b := watchLossBatch(9, false)
			if !b.FullRequired || b.Generation != 9 {
				t.Fatalf("loss=%+v", b)
			}
		})
	}
	b := watchRenameBatch("old/file", 9)
	if len(b.Paths) != 2 || b.Paths[1] != "old" || !b.FullRequired {
		t.Fatalf("rename=%+v", b)
	}
	if !watchLossBatch(9, true).RootChanged {
		t.Fatal("root flag lost")
	}
}
