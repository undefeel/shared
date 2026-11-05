package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func TestFolderPeerStateDurableEpochAndNamedSnapshot(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "state", "index.json")
	requireNoError(t, os.Mkdir(root, 0700))
	requireNoError(t, os.Mkdir(filepath.Dir(state), 0700))
	f, err := folder.Open(root, state, "A", nil)
	requireNoError(t, err)
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	writeTestFile(t, filepath.Join(root, "a"), []byte("a"), 0600)
	writeTestFile(t, filepath.Join(root, "b"), []byte("b"), 0600)
	mustScan(t, f)
	provider := folderPeerState{f}
	before := provider.Hello()
	_, epoch, cursor := f.DurableState()
	requireEqualf(t, before.StoreEpoch, epoch, "hello epoch = %q, durable = %q", before.StoreEpoch, epoch)
	named, err := provider.Index(context.Background(), epoch, cursor, 0, "b")
	if err != nil || len(named.Files) != 1 || named.Files[0].Name != "b" || named.Files[0].Disk != "" || named.Files[0].StoreDeletedAt != 0 {
		t.Fatalf("named result = %#v, %v", named, err)
	}
	baseline, err := provider.Index(context.Background(), "wrong-epoch", 0, 0, "")
	if err != nil || !baseline.Reset || len(baseline.Files) != 2 {
		t.Fatalf("baseline = %#v, %v", baseline, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Index(ctx, epoch, cursor, time.Second, ""); err == nil {
		t.Fatal("canceled wait succeeded")
	}
	requireNoError(t, f.Close())
	closed = true
	reopened, err := folder.Open(root, state, "A", nil)
	requireNoError(t, err)
	defer reopened.Close()
	if after := (folderPeerState{reopened}).Hello(); after.StoreEpoch != before.StoreEpoch {
		t.Fatalf("store epoch changed across reopen: %q -> %q", before.StoreEpoch, after.StoreEpoch)
	}
	_, _, reopenedCursor := reopened.DurableState()
	requireEqualf(t, reopenedCursor, cursor, "durable cursor changed across reopen: %d -> %d", cursor, reopenedCursor)
}
