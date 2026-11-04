package folder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func persistFolder(t *testing.T) *Folder {
	t.Helper()
	return &Folder{statePath: filepath.Join(t.TempDir(), "index.json"), files: map[string]*FileInfo{
		"a": {Name: "a", Disk: "a", Version: Vector{"me": 1}},
	}}
}
func readPersisted(t *testing.T, f *Folder) persisted {
	t.Helper()
	if f.store != nil {
		return storePersisted(f.store.state)
	}
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

func TestStoreDigestIndependentOfEncodingHistory(t *testing.T) {
	if os.Getenv("SHAREDIR_DIGEST_CHILD") == "1" {
		b, _ := encodeSnapshot(codecFixture())
		_ = b
		fmt.Printf("%x", storeDigest(storeFile{Name: "a", Version: []storePair{{"me", 1}}}))
		os.Exit(0)
	}
	want := fmt.Sprintf("%x", storeDigest(storeFile{Name: "a", Version: []storePair{{"me", 1}}}))
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreDigestIndependentOfEncodingHistory$")
	cmd.Env = append(os.Environ(), "SHAREDIR_DIGEST_CHILD=1")
	got, err := cmd.Output()
	if err != nil || string(got) != want {
		t.Fatalf("digest %s want %s: %v", got, want, err)
	}
}

func TestStoreReplayAndNoJSONRewrite(t *testing.T) {
	f := rootStateFixture(t)
	before, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Root, "a"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.statePath)
	if !bytes.Equal(before, after) {
		t.Fatal("JSON rewritten")
	}
	// A snapshot is authoritative even when the obsolete JSON is invalid.
	if err = os.WriteFile(f.statePath, []byte("invalid archive"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if fi, _, ok := g.Lookup("a"); !ok || fi.Hash == "" {
		t.Fatal("Open did not replay before serving lookup")
	}
	writes := 0
	g.SetPersistenceProbe(PersistenceProbe{Write: func(string, string, int, error) { writes++ }})
	gen := g.durableGeneration
	if err = g.Scan(); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || g.durableGeneration != gen {
		t.Fatal("no-op Scan wrote state")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.statePath), "index.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	data[11] = 99
	if err = os.WriteFile(filepath.Join(filepath.Dir(f.statePath), "index.snapshot"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(f.Root, f.statePath, "me", nil); err == nil {
		t.Fatal("unknown snapshot schema accepted")
	}
}

func TestLegacyIntentRecoveredBeforeMigration(t *testing.T) {
	f, j := journalFixture(t, 1)
	p := storePersisted(f.store.state)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.snapshot", "index.log"} {
		if err = os.Remove(filepath.Join(filepath.Dir(f.statePath), name)); err != nil {
			t.Fatal(err)
		}
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	journalBytes(t, g, j.Target, "old")
	if _, err = os.Stat(g.intentPath()); !os.IsNotExist(err) {
		t.Fatal("legacy intent remains", err)
	}
	if g.store == nil {
		t.Fatal("legacy index not migrated")
	}
}

func TestStoreConfirmEpochAndHistory(t *testing.T) {
	for _, action := range []string{"reset", "rebind"} {
		t.Run(action, func(t *testing.T) {
			f := rootStateFixture(t)
			if err := os.WriteFile(filepath.Join(f.Root, "a"), []byte("a"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.Scan(); err != nil {
				t.Fatal(err)
			}
			epoch := f.store.state.StoreID
			_ = f.blockRoot("root_empty", "test")
			token := f.Blocked().Token
			if err := f.Confirm(token, action, f.Root); err != nil {
				t.Fatal(err)
			}
			if (f.store.state.StoreID == epoch) != (action == "rebind") {
				t.Fatal("incorrect store epoch")
			}
			if err := f.Confirm(token, action, f.Root); !errors.Is(err, ErrStaleToken) {
				t.Fatal("stale token accepted", err)
			}
			g, err := Open(f.Root, f.statePath, "me", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			if g.Blocked() != nil || g.ConfirmedGeneration() != 1 {
				t.Fatal("confirmation lost")
			}
			_, _, exists := g.Lookup("a")
			if exists != (action == "rebind") {
				t.Fatal("incorrect retained history")
			}
		})
	}
}

func TestStoreDurabilityFailurePoisonsWriter(t *testing.T) {
	f := rootStateFixture(t)
	f.files["a"] = &FileInfo{Name: "a", Version: Vector{"me": 1}}
	generation := f.durableGeneration
	f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "G4" {
			return errors.New("flush failed")
		}
		return nil
	}})
	if err := f.save(); err == nil {
		t.Fatal("durability error reported success")
	}
	f.SetPersistenceProbe(PersistenceProbe{})
	if err := f.save(); err == nil || f.durableGeneration != generation {
		t.Fatal("uncertain writer reused")
	}
}

func TestResetInterruptedAfterMarkerRecovers(t *testing.T) {
	f := rootStateFixture(t)
	epoch := f.store.state.StoreID
	_ = f.blockRoot("root_empty", "test")
	token := f.Blocked().Token
	f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "reset" {
			return errors.New("power loss after reset marker")
		}
		return nil
	}})
	if err := f.Confirm(token, "reset", f.Root); err == nil {
		t.Fatal("failed reset reported success")
	}
	if f.Blocked() == nil {
		t.Fatal("failed confirmation opened latch")
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.store.state.StoreID == epoch || g.Blocked() != nil {
		t.Fatal("durable reset marker not completed")
	}
	for _, name := range []string{"index.snapshot", "index.log"} {
		if _, err = os.Stat(filepath.Join(filepath.Dir(f.statePath), name+".reset-"+token)); err != nil {
			t.Fatal("epoch archive missing", err)
		}
	}
}
