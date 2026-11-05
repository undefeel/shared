package folder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuiltinsBeforeScan(t *testing.T) {
	f := &Folder{}
	for _, name := range []string{"node_modules/a", "x/.venv/a", "x/__pycache__/a", ".next/cache", "dist/a", "x/dist/y", ".sharedir-tmp-x", "a/a.swp", "a.swo", "a~", ".#a", "#a#", "~$a", "a.crdownload", "a.part", "a.download"} {
		if !f.Ignored(name) {
			t.Errorf("%q must be excluded", name)
		}
	}
	for _, name := range []string{".git/config", "distance/a", "distfile", "my_node_modules/a", "a/venv/b", "a.part.txt", "/unmatched", "/"} {
		if f.Ignored(name) {
			t.Errorf("%q must be included", name)
		}
	}
	f.ignore = []string{"custom/*"}
	if !f.Ignored("custom/a") || !f.Ignored("dist/a") || f.Ignored("other/a") {
		t.Fatal("user rules must add restrictions to builtins")
	}
}

func TestIgnoredRecordsSurviveRestartAndRestore(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.write(t, "item", "keep", time.Now())
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	old, _, _ := n.f.Lookup("item")
	if err := os.WriteFile(filepath.Join(n.f.Root, ignoreFile), []byte("item\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	check := func(f *Folder) {
		t.Helper()
		fi, _, ok := f.Lookup("item")
		if !ok || fi.Deleted || !reflect.DeepEqual(old.Version, fi.Version) {
			t.Fatal("ignored record changed", fi)
		}
		snap, _ := f.Snapshot()
		durable, _, _ := f.DurableState()
		for _, view := range [][]FileInfo{snap, durable} {
			for _, v := range view {
				if v.Name == "item" {
					t.Fatal("ignored advertised")
				}
			}
		}
	}
	check(n.f)
	root, state := n.f.Root, n.f.statePath
	n.f.Close()
	f, err := Open(root, state, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	check(f)
	if err := os.Remove(filepath.Join(root, ignoreFile)); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.Snapshot()
	found := false
	for _, fi := range snap {
		if fi.Name == "item" && !fi.Deleted {
			found = true
		}
	}
	if !found {
		t.Fatal("record not restored")
	}
}

func TestApplyExcludedNamesNeverFetch(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	for _, name := range []string{"a/node_modules/b", "dist/a", "item.part"} {
		fi := FileInfo{Name: name, Hash: testIgnoreHash(), Size: 1, Version: Vector{"B": 1}}
		fetched := false
		applied, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{fi}, func(context.Context, FileInfo, io.Writer) error { fetched = true; return nil }, nil)
		if fetched || applied != 0 || failed != 0 {
			t.Fatalf("excluded apply %s: %d %d %v", name, applied, failed, fetched)
		}
		if _, err := os.Stat(filepath.Join(n.f.Root, name)); !os.IsNotExist(err) {
			t.Fatal("excluded installed", err)
		}
	}
}

func testIgnoreHash() string { return fmt.Sprintf("%x", sha256.Sum256([]byte("x"))) }

func TestApplyRechecksExclusionAfterDownload(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	fi := FileInfo{Name: "item", Hash: testIgnoreHash(), Size: 1, Version: Vector{"B": 1}}
	_, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{fi}, func(_ context.Context, _ FileInfo, w io.Writer) error {
		if _, err := io.WriteString(w, "x"); err != nil {
			return err
		}
		n.f.mu.Lock()
		n.f.ignore = []string{"item"}
		n.f.mu.Unlock()
		return nil
	}, nil)
	if failed != 0 {
		t.Fatal("ignored apply failed", failed)
	}
	if _, err := os.Stat(filepath.Join(n.f.Root, "item")); !os.IsNotExist(err) {
		t.Fatal("excluded target installed", err)
	}
	if _, _, ok := n.f.Lookup("item"); ok {
		t.Fatal("excluded target indexed")
	}
}

func TestBuiltinIgnoreReference(t *testing.T) {
	names := []string{"", ".", "/", "#", "##", "#x#", "a/#x#/z", ".hidden/file", "distfile", "x/node_modulesx/y", "x/a.part.txt", "a/../dist/x", "/a/.DS_Store/x"}
	for _, rule := range builtinIgnore {
		sample := strings.ReplaceAll(rule, "*", "sample")
		names = append(names, sample, "a/b/"+sample+"/c", "prefix"+sample+"suffix", ".hidden/"+sample, sample+"x")
	}
	users := [][]string{nil, {"custom/*", "a/?", "[", "x/y", "*.go", "#*#"}}
	for _, user := range users {
		for _, name := range names {
			old := append(append([]string(nil), builtinIgnore...), user...)
			if got, want := ignoredWith(user, name), matchIgnore(old, name); got != want {
				t.Errorf("name %q user %q: got %v want %v", name, user, got, want)
			}
		}
	}
}

func BenchmarkIgnore(b *testing.B) {
	names := []string{"ordinary/deep/path/file.txt", "ordinary/node_modules/package/a", "src/foo.go", "a/b/file.swp"}
	user := []string{"custom/*", "*.log"}
	for _, old := range []bool{false, true} {
		label := "optimized"
		if old {
			label = "reference"
		}
		b.Run(label, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				name := names[i%len(names)]
				if old {
					matchIgnore(append(append([]string(nil), builtinIgnore...), user...), name)
				} else {
					ignoredWith(user, name)
				}
			}
		})
	}
}

func TestDurableCursorMatchesSnapshot(t *testing.T) {
	n := newNode(t, "cursor")
	defer n.f.Close()
	check := func() {
		t.Helper()
		_, epoch, cursor := n.f.DurableState()
		gotEpoch, gotCursor := n.f.DurableCursor()
		if epoch != gotEpoch || cursor != gotCursor {
			t.Fatalf("cursor = %q/%d, snapshot = %q/%d", gotEpoch, gotCursor, epoch, cursor)
		}
	}
	check()
	n.write(t, "file", "content", time.Now())
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	check()
}
