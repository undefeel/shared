package folder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func journalFixture(t *testing.T, phase int) (*Folder, journalIntent) {
	t.Helper()
	f := newNode(t, "local").f
	t.Cleanup(func() { f.Close() })
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	before := &FileInfo{Name: "a", Hash: hash("old"), Version: Vector{"local": 1}}
	after := &FileInfo{Name: "a", Hash: hash("new"), Version: Vector{"peer": 1}}
	if err := os.WriteFile(filepath.Join(f.Root, "a"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	f.files["a"] = before
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	c, err := f.root.prepareCapture("a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	file, err := f.root.openRead("a")
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	err = unix.Fstat(int(file.Fd()), &st)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	j := journalIntent{TxID: "test", Generation: f.durableGeneration + 1, Binding: f.binding, Before: before, After: after, Target: "a", Capture: c.Recovery, Identity: captureIdentity{uint64(st.Dev), uint64(st.Ino)}}
	if err = f.writeIntent(j); err != nil {
		t.Fatal(err)
	}
	if phase >= 1 {
		if err = c.take(); err != nil {
			t.Fatal(err)
		}
	}
	if phase >= 2 {
		if err = os.WriteFile(filepath.Join(f.Root, "a"), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if phase >= 3 {
		f.files["a"] = after
		if err = f.save(); err != nil {
			t.Fatal(err)
		}
	}
	return f, j
}
func recoverJournal(t *testing.T, f *Folder) {
	t.Helper()
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if err := f.recoverIntent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.intentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains: %v", err)
	}
}
func journalBytes(t *testing.T, f *Folder, name, want string) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(f.Root, name))
	if e != nil || string(b) != want {
		t.Fatalf("%s: %q %v", name, b, e)
	}
}
func TestJournalCrashAfterIntent(t *testing.T) {
	f, _ := journalFixture(t, 0)
	recoverJournal(t, f)
	journalBytes(t, f, "a", "old")
}
func TestJournalCrashAfterCapture(t *testing.T) {
	f, _ := journalFixture(t, 1)
	recoverJournal(t, f)
	journalBytes(t, f, "a", "old")
}
func TestJournalCrashAfterInstall(t *testing.T) {
	f, _ := journalFixture(t, 2)
	recoverJournal(t, f)
	journalBytes(t, f, "a", "new")
	entries, e := os.ReadDir(f.Root)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, ent := range entries {
		if ent.Name() != "a" {
			journalBytes(t, f, ent.Name(), "old")
			found = true
		}
	}
	if !found {
		t.Fatal("captured bytes lost")
	}
}
func TestJournalCrashAfterIndex(t *testing.T) {
	f, j := journalFixture(t, 3)
	recoverJournal(t, f)
	journalBytes(t, f, "a", "new")
	if _, e := os.Stat(filepath.Join(f.Root, j.Capture)); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("capture remains: %v", e)
	}
}
func TestJournalRepeatedRecovery(t *testing.T) {
	f, _ := journalFixture(t, 1)
	recoverJournal(t, f)
	recoverJournal(t, f)
	journalBytes(t, f, "a", "old")
}
func TestJournalAmbiguousHashBlocks(t *testing.T) {
	f, _ := journalFixture(t, 0)
	if e := os.WriteFile(filepath.Join(f.Root, "a"), []byte("unknown"), 0600); e != nil {
		t.Fatal(e)
	}
	f.scanMu.Lock()
	e := f.recoverIntent()
	f.scanMu.Unlock()
	if e == nil || f.Blocked() == nil {
		t.Fatal("ambiguous recovery accepted")
	}
	journalBytes(t, f, "a", "unknown")
}
func TestJournalMaliciousPaths(t *testing.T) {
	for _, bad := range []string{"../outside", "/outside", "a/../outside", "a\\b", ".", "e\x00x"} {
		for _, field := range []string{"target", "capture", "tmp"} {
			t.Run(field+bad, func(t *testing.T) {
				f, j := journalFixture(t, 0)
				switch field {
				case "target":
					j.Target = bad
				case "capture":
					j.Capture = bad
				case "tmp":
					j.Tmp = bad
				}
				b, _ := json.Marshal(j)
				if e := os.WriteFile(f.intentPath(), b, 0600); e != nil {
					t.Fatal(e)
				}
				if e := f.recoverIntent(); e == nil {
					t.Fatal("malicious intent accepted")
				}
				journalBytes(t, f, "a", "old")
			})
		}
	}
}
func TestJournalReplacedCaptureBecomesConflict(t *testing.T) {
	f, j := journalFixture(t, 3)
	held, e := f.root.openRead(j.Capture)
	if e != nil {
		t.Fatal(e)
	}
	defer held.Close()
	if e = os.Remove(filepath.Join(f.Root, j.Capture)); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(f.Root, j.Capture), []byte("external"), 0600); e != nil {
		t.Fatal(e)
	}
	recoverJournal(t, f)
	if f.Blocked() != nil {
		t.Fatal("replacement blocked folder")
	}
	entries, e := os.ReadDir(f.Root)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, ent := range entries {
		if ent.Name() != "a" {
			journalBytes(t, f, ent.Name(), "external")
			found = true
		}
	}
	if !found {
		t.Fatal("replacement lost")
	}
}
