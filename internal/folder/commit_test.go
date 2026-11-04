package folder

import (
	"errors"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
)

func commitFixture(t *testing.T) (*Folder, *commitTree, persisted) {
	t.Helper()
	f, j := journalFixture(t, 0)
	c, err := f.root.openCapture(j.Target, j.Capture, j.Identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	incoming, err := f.root.create(tmpPrefix + "incoming")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { incoming.Close() })
	if _, err = incoming.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	j.Tmp = tmpPrefix + "incoming"
	tree := &commitTree{intent: j, incoming: incoming, capture: c, dirs: []*os.File{c.parent}}
	tree.mutate = func() error { return renameNoReplace(c.parent, path.Base(j.Tmp), c.parent, path.Base(j.Target)) }
	return f, tree, persisted{Generation: j.Generation, Clock: f.clock, Files: []*FileInfo{j.After}}
}

func runCommit(f *Folder, tree *commitTree, p persisted, publish func()) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.commitLocked(p, tree, publish)
}

func TestCommitFlushOrder(t *testing.T) {
	f, tree, p := commitFixture(t)
	var trace []string
	f.persistHooks.syncFile = func(file *os.File) error {
		label := "index"
		if file == tree.incoming {
			label = "data"
		} else if strings.HasPrefix(path.Base(file.Name()), ".sharedir-intent-") {
			label = "intent"
		}
		trace = append(trace, label)
		return nil
	}
	dirs := 0
	f.persistHooks.syncDir = func(d *os.File) error {
		dirs++
		trace = append(trace, "dir")
		if dirs == 5 { // Cleanup parent: both recovery objects must already be gone.
			for _, name := range []string{f.intentPath(), f.Root + "/" + tree.capture.Recovery} {
				if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cleanup %s: %v", name, err)
				}
			}
		}
		return nil
	}
	if err := runCommit(f, tree, p, func() { trace = append(trace, "publish") }); err != nil {
		t.Fatal(err)
	}
	want := []string{"data", "intent", "dir", "dir", "dir", "index", "dir", "dir", "dir", "publish"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace %v", trace)
	}
}

func commitSyncFailure(t *testing.T, fail int) {
	t.Helper()
	f, tree, p := commitFixture(t)
	_, seq := f.Snapshot()
	ch := f.changed
	calls := 0
	published := false
	fault := errors.New("sync fault")
	hook := func(*os.File) error {
		calls++
		if calls == fail {
			return fault
		}
		return nil
	}
	f.persistHooks.syncFile = hook
	f.persistHooks.syncDir = hook
	err := runCommit(f, tree, p, func() { published = true; f.notify() })
	if !errors.Is(err, fault) || published {
		t.Fatalf("err=%v published=%v", err, published)
	}
	_, after := f.Snapshot()
	if seq != after {
		t.Fatal("seq advanced")
	}
	select {
	case <-ch:
		t.Fatal("notified before successful commit")
	default:
	}
}
func TestCommitDataSyncFailure(t *testing.T)          { commitSyncFailure(t, 1) }
func TestCommitIntentSyncFailure(t *testing.T)        { commitSyncFailure(t, 2) }
func TestCommitIntentDirSyncFailure(t *testing.T)     { commitSyncFailure(t, 3) }
func TestCommitCaptureDirSyncFailure(t *testing.T)    { commitSyncFailure(t, 4) }
func TestCommitInstallDirSyncFailure(t *testing.T)    { commitSyncFailure(t, 5) }
func TestCommitIndexSyncFailure(t *testing.T)         { commitSyncFailure(t, 6) }
func TestCommitIndexDirSyncFailure(t *testing.T)      { commitSyncFailure(t, 7) }
func TestCommitCleanupParentSyncFailure(t *testing.T) { commitSyncFailure(t, 8) }
func TestCommitCleanupStateSyncFailure(t *testing.T)  { commitSyncFailure(t, 9) }

func TestCommitOpenRecoversBeforeScan(t *testing.T) {
	f, j := journalFixture(t, 1)
	reopened, err := Open(f.Root, f.statePath, f.me, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	data, err := os.ReadFile(f.Root + "/" + j.Target)
	if err != nil || string(data) != "old" {
		t.Fatalf("restored %q: %v", data, err)
	}
	if err = reopened.Scan(); err != nil {
		t.Fatal(err)
	}
	fi, _, ok := reopened.Lookup(j.Target)
	if !ok || fi.Deleted {
		t.Fatal("recovery produced deletion")
	}
}

func TestScanPublishesAfterDurableIndex(t *testing.T) {
	f := newNode(t, "local").f
	defer f.Close()
	if err := os.WriteFile(f.Root+"/new", []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	before, seq := f.Snapshot()
	ch := f.changed
	flushed := false
	f.persistHooks.syncDir = func(d *os.File) error {
		current, s := f.Snapshot()
		if !reflect.DeepEqual(before, current) || s != seq {
			t.Fatal("published before directory sync")
		}
		select {
		case <-ch:
			t.Fatal("early notify")
		default:
		}
		if err := syncDir(d); err != nil {
			return err
		}
		flushed = true
		return nil
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	current, s := f.Snapshot()
	if !flushed || len(current) != 1 || s == seq {
		t.Fatal("durable scan was not published")
	}
}

func TestCommitOpenCleansCommittedCapture(t *testing.T) {
	f, j := journalFixture(t, 3)
	reopened, err := Open(f.Root, f.statePath, f.me, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, name := range []string{f.intentPath(), f.Root + "/" + j.Capture} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("left recovery object %s: %v", name, err)
		}
	}
}
