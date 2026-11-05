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

func TestIncomingBytesPreserveMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		exec bool
	}{{"0600", 0o600, false}, {"0700", 0o700, true}} {
		t.Run(tc.name, func(t *testing.T) {
			n := applySeed(t)
			defer n.f.Close()
			if err := os.Chmod(filepath.Join(n.f.Root, "x"), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := n.f.Scan(); err != nil {
				t.Fatal(err)
			}
			applyModeRemote(t, n, modeRemote("updated", tc.exec), bodyFetch("updated"))
			if got := filePerm(t, n); got != tc.mode {
				t.Fatalf("mode = %04o, want %04o", got, tc.mode)
			}
		})
	}
}

func TestNewIncomingMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		exec bool
	}{{"0644", 0o644, false}, {"0755", 0o755, true}} {
		t.Run(tc.name, func(t *testing.T) {
			n := applySeed(t)
			defer n.f.Close()
			r := modeRemote("new", tc.exec)
			r.Name = "new"
			applyModeRemote(t, n, r, bodyFetch("new"))
			if got, err := os.Stat(filepath.Join(n.f.Root, "new")); err != nil || got.Mode().Perm() != tc.mode {
				t.Fatalf("stat=%v, err=%v; want mode %04o", got, err, tc.mode)
			}
		})
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

func TestExecMetadataOnlyDoesNotFetch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after os.FileMode
		exec          bool
	}{{"true", 0o600, 0o700, true}, {"false", 0o700, 0o600, false}} {
		t.Run(tc.name, func(t *testing.T) {
			n := applySeed(t)
			defer n.f.Close()
			if err := os.Chmod(filepath.Join(n.f.Root, "x"), tc.before); err != nil {
				t.Fatal(err)
			}
			if err := n.f.Scan(); err != nil {
				t.Fatal(err)
			}
			applyModeRemote(t, n, modeRemote("old", tc.exec), func(context.Context, FileInfo, io.Writer) error { t.Fatal("unexpected fetch"); return nil })
			if got := filePerm(t, n); got != tc.after {
				t.Fatalf("mode = %04o, want %04o", got, tc.after)
			}
		})
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
