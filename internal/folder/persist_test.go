package folder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func persistFolder(t *testing.T) *Folder {
	t.Helper()
	return &Folder{statePath: filepath.Join(t.TempDir(), "index.json"), files: map[string]*FileInfo{
		"a": {Name: "a", Disk: "local-a", Version: Vector{"me": 1}},
	}}
}
func readPersisted(t *testing.T, f *Folder) persisted {
	t.Helper()
	b, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestSaveParallelVectorMutation(t *testing.T) {
	f := persistFolder(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 40 {
				f.mu.Lock()
				f.files["a"].Version["me"]++
				f.clock++
				f.mu.Unlock()
				if err := f.save(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	if p := readPersisted(t, f); p.Files[0].Version["me"] != 161 {
		t.Fatal(p)
	}
}
func TestSaveDelayedOldWriter(t *testing.T) {
	f := persistFolder(t)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	f.persistHooks.marshal = func(v any) ([]byte, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return json.Marshal(v)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.save() }()
	<-entered
	f.mu.Lock()
	f.files["a"].Version["me"] = 2
	f.mu.Unlock()
	go func() { second <- f.save() }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	p := readPersisted(t, f)
	if p.Files[0].Version["me"] != 2 || p.Generation < 2 {
		t.Fatal(p)
	}
}
func testSaveFailure(t *testing.T, hook string) {
	t.Helper()
	f := persistFolder(t)
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	before := readPersisted(t, f)
	boom := errors.New("injected " + hook)
	switch hook {
	case "marshal":
		f.persistHooks.marshal = func(any) ([]byte, error) { return nil, boom }
	case "rename":
		f.persistHooks.rename = func(string, string) error { return boom }
	case "file sync":
		f.persistHooks.syncFile = func(*os.File) error { return boom }
	case "dir sync":
		f.persistHooks.syncDir = func(*os.File) error { return boom }
	}
	f.mu.Lock()
	f.files["a"].Version["me"] = 2
	f.mu.Unlock()
	if err := f.save(); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if f.durableGeneration != before.Generation {
		t.Fatal("failed save marked durable")
	}
	p := readPersisted(t, f)
	if hook != "dir sync" && p.Files[0].Version["me"] != 1 {
		t.Fatal("old index replaced")
	}
	tmp, err := filepath.Glob(filepath.Join(filepath.Dir(f.statePath), ".sharedir-index-*"))
	if err != nil || len(tmp) != 0 {
		t.Fatalf("temporary files: %v %v", tmp, err)
	}
	f.persistHooks = persistHooks{}
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	p = readPersisted(t, f)
	if p.Generation <= before.Generation || p.Files[0].Version["me"] != 2 {
		t.Fatal(p)
	}
}
func TestSaveMarshalFailure(t *testing.T)       { testSaveFailure(t, "marshal") }
func TestSaveRenameFailure(t *testing.T)        { testSaveFailure(t, "rename") }
func TestSaveFileSyncFailure(t *testing.T)      { testSaveFailure(t, "file sync") }
func TestSaveDirectorySyncFailure(t *testing.T) { testSaveFailure(t, "dir sync") }
func TestSaveReadableJSONMonotonicGeneration(t *testing.T) {
	f := persistFolder(t)
	var previous uint64
	for range 3 {
		if err := f.save(); err != nil {
			t.Fatal(err)
		}
		p := readPersisted(t, f)
		if p.Generation <= previous {
			t.Fatal("generation regressed")
		}
		previous = p.Generation
	}
	reopened, err := Open(t.TempDir(), f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.save(); err != nil {
		t.Fatal(err)
	}
	if readPersisted(t, reopened).Generation <= previous {
		t.Fatal("generation reset on restart")
	}
}
func TestSnapshotVectorRace(t *testing.T) {
	f := persistFolder(t)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			f.mu.Lock()
			f.files["a"].Version["me"]++
			f.mu.Unlock()
		}
	})
	for range 1000 {
		s, _ := f.Snapshot()
		s[0].Version["me"] = 0
		if s[0].Disk != "" {
			t.Fatal("Disk leaked")
		}
		fi, _, ok := f.Lookup("a")
		if !ok {
			t.Fatal("missing record")
		}
		fi.Version["me"] = 0
	}
	wg.Wait()
	if f.files["a"].Version["me"] != 1001 {
		t.Fatal("shared Vector")
	}
}

func TestSaveUniqueTemporaryFile(t *testing.T) {
	f := persistFolder(t)
	names := map[string]bool{}
	f.persistHooks.syncFile = func(file *os.File) error {
		if names[file.Name()] {
			t.Error("temporary name reused")
		}
		names[file.Name()] = true
		st, err := file.Stat()
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0600 {
			t.Errorf("temporary mode %o", st.Mode().Perm())
		}
		return syncFile(file)
	}
	for range 2 {
		if err := f.save(); err != nil {
			t.Fatal(err)
		}
	}
}
