package folder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func committedOwnEvents(t *testing.T) (*Folder, groupIntent) {
	t.Helper()
	f, g, trees, dirs := groupCommitFixture(t)
	if err := runGroupCommit(f, g, trees, dirs, func() {
		for _, e := range g.Entries {
			f.files[e.After.Name] = e.After
		}
	}); err != nil {
		t.Fatal(err)
	}
	return f, g
}

func TestFilterWatchBatchExactOwnEvent(t *testing.T) {
	f, g := committedOwnEvents(t)
	defer f.Close()
	got := f.FilterWatchBatch(context.Background(), WatchBatch{Paths: []string{"a"}, Generation: g.ConfirmedGeneration})
	if len(got.Paths) != 0 || got.FullRequired || got.RootChanged {
		t.Fatalf("exact own event was not suppressed: %+v", got)
	}
}

func TestFilterWatchBatchExternalSameMtimeSurvives(t *testing.T) {
	f, g := committedOwnEvents(t)
	defer f.Close()
	name := filepath.Join(f.Root, "a")
	st, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	mtime := st.ModTime()
	if err = os.WriteFile(name, []byte("new x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(name, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	got := f.FilterWatchBatch(context.Background(), WatchBatch{Paths: []string{"a"}, Generation: g.ConfirmedGeneration})
	if len(got.Paths) != 1 || got.Paths[0] != "a" {
		t.Fatalf("external edit was suppressed: %+v", got)
	}
}

func TestFilterWatchBatchMixedRootAndStaleGeneration(t *testing.T) {
	f, g := committedOwnEvents(t)
	defer f.Close()
	for _, tc := range []struct {
		name      string
		batch     WatchBatch
		wantPaths int
	}{
		{name: "mixed", batch: WatchBatch{Paths: []string{"a", "external"}, Generation: g.ConfirmedGeneration}, wantPaths: 1},
		{name: "root", batch: WatchBatch{Paths: []string{"a"}, RootChanged: true, FullRequired: true, Generation: g.ConfirmedGeneration}, wantPaths: 1},
		{name: "stale", batch: WatchBatch{Paths: []string{"a"}, Generation: g.ConfirmedGeneration + 1}, wantPaths: 1},
		{name: "empty root path", batch: WatchBatch{Paths: []string{"."}, Generation: g.ConfirmedGeneration}, wantPaths: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.FilterWatchBatch(context.Background(), tc.batch)
			if len(got.Paths) != tc.wantPaths {
				t.Fatalf("paths=%v", got.Paths)
			}
			if tc.batch.FullRequired && (!got.FullRequired || !got.RootChanged) {
				t.Fatalf("root flags lost: %+v", got)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := f.FilterWatchBatch(ctx, WatchBatch{Paths: []string{"a"}, Generation: g.ConfirmedGeneration})
	if len(got.Paths) != 1 {
		t.Fatalf("canceled context suppressed path: %+v", got)
	}
}

func TestOwnRenameAndIndependentFull(t *testing.T) {
	f, g := committedOwnEvents(t)
	defer f.Close()
	b := f.FilterWatchBatch(context.Background(), watchRenameBatch("a", g.ConfirmedGeneration))
	if b.FullRequired || len(b.Paths) != 0 || len(b.RenamePaths) != 0 {
		t.Fatalf("own rename dirty: %+v", b)
	}
	b = watchRenameBatch("a", g.ConfirmedGeneration)
	b.RenameFull = false
	if got := f.FilterWatchBatch(context.Background(), b); !got.FullRequired {
		t.Fatal("hard full suppressed")
	}
	b = f.FilterWatchBatch(context.Background(), watchRenameBatch("unknown", g.ConfirmedGeneration))
	if !b.FullRequired {
		t.Fatal("unknown rename lost")
	}
}
