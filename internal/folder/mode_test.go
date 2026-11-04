package folder

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func modeRemote(body string, exec bool) FileInfo {
	r := incoming(body, Vector{"A": 100, "B": 1}, time.Now().Add(time.Hour).UnixNano())
	r.Exec = exec
	return r
}

func applyModeRemote(t *testing.T, n *node, r FileInfo, fetch Fetcher) {
	t.Helper()
	if _, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, fetch, nil); failed != 0 {
		t.Fatalf("ApplyRemote failed: %d", failed)
	}
}

func filePerm(t *testing.T, n *node) os.FileMode {
	t.Helper()
	st, err := os.Stat(filepath.Join(n.f.Root, "x"))
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestIncomingBytesPreserve0600(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	if err := os.Chmod(filepath.Join(n.f.Root, "x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	r := modeRemote("updated", false)
	applyModeRemote(t, n, r, bodyFetch("updated"))
	if got := filePerm(t, n); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
}

func TestIncomingBytesPreserve0700(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	if err := os.Chmod(filepath.Join(n.f.Root, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	r := modeRemote("updated", true)
	applyModeRemote(t, n, r, bodyFetch("updated"))
	if got := filePerm(t, n); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

func TestNewIncomingMode0644(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := modeRemote("new", false)
	r.Name = "new"
	applyModeRemote(t, n, r, bodyFetch("new"))
	if got, err := os.Stat(filepath.Join(n.f.Root, "new")); err != nil || got.Mode().Perm() != 0o644 {
		t.Fatalf("stat=%v, err=%v; want mode 0644", got, err)
	}
}

func TestNewIncomingMode0755(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := modeRemote("new", true)
	r.Name = "new"
	applyModeRemote(t, n, r, bodyFetch("new"))
	if got, err := os.Stat(filepath.Join(n.f.Root, "new")); err != nil || got.Mode().Perm() != 0o755 {
		t.Fatalf("stat=%v, err=%v; want mode 0755", got, err)
	}
}

func TestChmodOnlyWithSameMtimeAdvancesVector(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	before, _ := n.f.Snapshot()
	path := filepath.Join(n.f.Root, "x")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	after, _ := n.f.Snapshot()
	if before[0].ModTime != after[0].ModTime {
		t.Fatalf("mtime changed: %d -> %d", before[0].ModTime, after[0].ModTime)
	}
	if after[0].Version.Compare(before[0].Version) != Greater || !after[0].Exec {
		t.Fatalf("chmod change not versioned: before=%+v after=%+v", before[0], after[0])
	}
}

func TestExecTrueMetadataOnlyDoesNotFetch(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	if err := os.Chmod(filepath.Join(n.f.Root, "x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	r := modeRemote("old", true)
	applyModeRemote(t, n, r, func(context.Context, FileInfo, io.Writer) error { t.Fatal("unexpected fetch"); return nil })
	if got := filePerm(t, n); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

func TestExecFalseMetadataOnlyDoesNotFetch(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	if err := os.Chmod(filepath.Join(n.f.Root, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	r := modeRemote("old", false)
	applyModeRemote(t, n, r, func(context.Context, FileInfo, io.Writer) error { t.Fatal("unexpected fetch"); return nil })
	if got := filePerm(t, n); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
}

func TestEqualHashDifferentExecIsNotMergeOnly(t *testing.T) {
	f := &Folder{me: "A"}
	l := incoming("same", Vector{"A": 1}, 1)
	r := incoming("same", Vector{"B": 1}, 2)
	r.Exec = true
	if got := f.decide(&l, &r, "B"); got == mergeOnly {
		t.Fatal("equal hash with different Exec was merged")
	}
}
