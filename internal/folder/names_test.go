package folder

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalNameValidation(t *testing.T) {
	if !validName("café/file.txt") || validName("cafe\u0301/file.txt") {
		t.Fatal("canonical names must be NFC")
	}
	for _, name := range []string{"", ".", "../x", "a/../x", "/x", "a\\b", "a\x00b"} {
		if validName(name) {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestScanMapsSingleNFDFile(t *testing.T) {
	n := newNode(t, "local")
	disk := "cafe\u0301.txt"
	if err := os.WriteFile(filepath.Join(n.f.Root, disk), []byte("decomposed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	fi, _, ok := n.f.Lookup("café.txt")
	if !ok || fi.Name != "café.txt" || fi.Disk != disk {
		t.Fatalf("unexpected NFD mapping: %#v, found=%v", fi, ok)
	}
}

type namedEntry string

func (e namedEntry) Name() string               { return string(e) }
func (e namedEntry) IsDir() bool                { return false }
func (e namedEntry) Type() os.FileMode          { return 0 }
func (e namedEntry) Info() (os.FileInfo, error) { return nil, nil }

func TestScanNFCNFDCollisionKeepsPublishedCopies(t *testing.T) {
	n := newNode(t, "local")
	if err := os.WriteFile(filepath.Join(n.f.Root, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	base := n.f.rootScanIO()
	ioops := base
	meta := scanStat{size: 1}
	n.f.hashCache = map[string]scanHashCache{
		"café.txt":       {scanStat: meta, hash: "same-stat-nfc-bytes"},
		"cafe\u0301.txt": {scanStat: meta, hash: "same-stat-nfd-bytes"},
	}
	ioops.readDir = func(*os.File) ([]os.DirEntry, error) {
		return []os.DirEntry{namedEntry("café.txt"), namedEntry("cafe\u0301.txt")}, nil
	}
	ioops.info = func(_ *os.File, e os.DirEntry) (os.FileInfo, scanStat, error) {
		return scanFileInfo{name: e.Name(), size: 1}, meta, nil
	}
	err := n.f.scanWith(ioops)
	if err == nil || !strings.Contains(err.Error(), "café.txt") || !strings.Contains(err.Error(), "cafe\u0301.txt") {
		t.Fatalf("expected collision diagnostic naming both paths, got %v", err)
	}
	fi, _, ok := n.f.Lookup("keep.txt")
	if !ok || fi.Deleted {
		t.Fatal("failed candidate scan changed the published copy")
	}
}

func TestNFDIncomingRejectedBeforeFetch(t *testing.T) {
	n := newNode(t, "local")
	fetched := false
	remote := []FileInfo{{Name: "cafe\u0301.txt", Version: Vector{"peer": 1}}}
	_, failed := n.f.ApplyRemote(context.Background(), "peer", remote, func(context.Context, FileInfo, io.Writer) error {
		fetched = true
		return nil
	}, nil)
	if failed != 1 || fetched {
		t.Fatalf("invalid incoming name: failed=%d fetched=%v", failed, fetched)
	}
}

func writeLegacyIndex(t *testing.T, state string, files []*FileInfo) {
	t.Helper()
	b, err := json.Marshal(persisted{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsDuplicateLegacyNamesWithoutRemovingFiles(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "data"), filepath.Join(dir, "index.json")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "copy.txt")
	if err := os.WriteFile(file, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyIndex(t, state, []*FileInfo{{Name: "copy.txt"}, {Name: "copy.txt"}})
	if _, err := Open(root, state, "local", nil); err == nil {
		t.Fatal("duplicate legacy names accepted")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "preserve" {
		t.Fatalf("legacy validation removed a copy: %q, %v", b, err)
	}
}

func TestOpenRejectsMaliciousLegacyDiskWithoutRemovingFiles(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "data"), filepath.Join(dir, "index.json")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(file, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyIndex(t, state, []*FileInfo{{Name: "safe.txt", Disk: "../outside.txt"}})
	if _, err := Open(root, state, "local", nil); err == nil {
		t.Fatal("malicious legacy Disk accepted")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "preserve" {
		t.Fatalf("legacy validation removed a copy: %q, %v", b, err)
	}
}

func TestValidDiskRequiresSameCanonicalName(t *testing.T) {
	nfd := "é.txt" // NFD для "é.txt"
	if !validDisk("é.txt", nfd) {
		t.Fatal("NFD-путь того же имени должен проходить")
	}
	if validDisk("a.txt", "b.txt") {
		t.Fatal("Disk другого имени не должен проходить")
	}
	if validDisk("a.txt", "dir/a.txt") {
		t.Fatal("Disk в другом каталоге не должен проходить")
	}
}
