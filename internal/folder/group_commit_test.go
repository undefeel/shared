package folder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func groupCommitFixture(t *testing.T) (*Folder, groupIntent, []*commitTree, []*os.File) {
	t.Helper()
	f := newNode(t, "group").f
	t.Cleanup(func() { f.Close() })
	g := groupIntent{Schema: storeSchema, StoreID: f.store.state.StoreID, TxID: [16]byte{2}, BaseGeneration: f.store.state.Generation, Generation: f.store.state.Generation + 1, Clock: f.clock, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration}
	parent, err := os.Open(f.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })
	var trees []*commitTree
	for _, name := range []string{"a", "b"} {
		tmp := tmpPrefix + name
		file, err := f.root.create(tmp)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		body := "new " + name
		if _, err = file.WriteString(body); err != nil {
			t.Fatal(err)
		}
		dev, ino, err := fileDevice(file)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(body))
		after := &FileInfo{Name: name, Hash: hex.EncodeToString(hash[:]), Size: int64(len(body)), Version: Vector{"peer": 1}}
		g.Entries = append(g.Entries, groupIntentEntry{Target: name, Tmp: tmp, After: after, IncomingIdentity: captureIdentity{dev, ino}})
		trees = append(trees, &commitTree{incoming: file, dirs: []*os.File{parent, parent}, mutate: func() error { return renameNoReplace(parent, tmp, parent, name) }})
	}
	return f, g, trees, []*os.File{parent, parent}
}
func runGroupCommit(f *Folder, g groupIntent, trees []*commitTree, dirs []*os.File, publish func()) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.commitGroupLocked(g, trees, dirs, publish)
}

func TestGroupCommitTraceAndEveryFlush(t *testing.T) {
	// First record actual flush attempts; run the same transaction with each one
	// failing, including hardware flushes on platforms that support them.
	total := 0
	durableAt := 0
	for fail := -1; fail < total; fail++ {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f, g, trees, dirs := groupCommitFixture(t)
			var phases []string
			var flushes []string
			published := false
			calls := 0
			base := f.durableGeneration
			fault := errors.New("flush fault")
			f.SetPersistenceProbe(PersistenceProbe{
				Phase: func(_, phase string) {
					phases = append(phases, phase)
					if phase == "G5" && !published {
						t.Fatal("cleanup before publication")
					}
				},
				Fault: func(kind, phase string, _, _ uint64) error {
					calls++
					flushes = append(flushes, phase+":"+kind)
					if phase == "G4" {
						durableAt = calls
					}
					if calls == fail+1 {
						return fault
					}
					return nil
				},
			})
			err := runGroupCommit(f, g, trees, dirs, func() {
				if f.store.state.Generation != g.Generation || len(f.store.state.Files) != 2 {
					t.Fatal("partial durable publication")
				}
				for _, e := range g.Entries {
					f.files[e.Target] = e.After
				}
				published = true
			})
			if fail == -1 {
				if err != nil {
					t.Fatal(err)
				}
				total = calls
				if !reflect.DeepEqual(phases, []string{"G1", "G2", "G3", "G4", "G5"}) {
					t.Fatal(phases)
				}
				if flushes[0] != "G1:fsync" || flushes[1] != "G1:fsync" || flushes[2] != "G1:fsync" {
					t.Fatal(flushes)
				}
				counts := map[string]int{}
				for _, s := range flushes {
					counts[s]++
				}
				if counts["G1:fsync"] != 3 || counts["G3b:fsync"] != 1 || counts["G1:FULLFSYNC"] > 1 {
					t.Fatal(counts)
				}
			} else if fail+1 <= durableAt {
				if !errors.Is(err, fault) || published || f.Blocked() == nil || f.durableGeneration != base {
					t.Fatalf("err=%v published=%v blocked=%v", err, published, f.Blocked())
				}
			} else {
				if err != nil || !published || f.Blocked() != nil {
					t.Fatalf("cleanup undid commit: %v", err)
				}
				if _, err = os.Stat(f.intentPath()); err != nil {
					t.Fatal("missing retry marker", err)
				}
			}
			if published {
				for _, name := range []string{"a", "b"} {
					b, e := os.ReadFile(filepath.Join(f.Root, name))
					if e != nil || string(b) != "new "+name {
						t.Fatalf("%s %q %v", name, b, e)
					}
				}
			}
		})
	}
}

func TestGroupCommitMutateFailure(t *testing.T) {
	f, g, trees, dirs := groupCommitFixture(t)
	base := f.store.state.Generation
	fault := errors.New("mutate fault")
	trees[1].mutate = func() error { return fault }
	if err := runGroupCommit(f, g, trees, dirs, func() { t.Fatal("partial publication") }); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if f.store.state.Generation != base {
		t.Fatal("partial commit")
	}
	if _, err := os.Stat(f.intentPath()); err != nil {
		t.Fatal("intent lost", err)
	}
	if b, err := os.ReadFile(filepath.Join(f.Root, "a")); err != nil || string(b) != "new a" {
		t.Fatal("installed data lost")
	}
}

func TestGroupCommitCleanupRetry(t *testing.T) {
	f, g, trees, dirs := groupCommitFixture(t)
	fault := errors.New("cleanup fault")
	f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "intent-dir" && f.durableGeneration == g.Generation {
			return fault
		}
		return nil
	}})
	if err := runGroupCommit(f, g, trees, dirs, func() {}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.intentPath())
	if err != nil {
		t.Fatal(err)
	}
	next := g
	next.TxID = [16]byte{3}
	next.BaseGeneration++
	next.Generation++
	// Cleanup fault prevents any second intent from replacing the first.
	if err = runGroupCommit(f, next, trees, dirs, func() { t.Fatal("second publish") }); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.intentPath())
	if err != nil || string(b) != string(original) {
		t.Fatal("marker overwritten")
	}
	f.SetPersistenceProbe(PersistenceProbe{})
	// Before mismatch rejects the new preparation, but only after old cleanup.
	if err = runGroupCommit(f, next, trees, dirs, func() { t.Fatal("stale preparation") }); err == nil {
		t.Fatal("accepted stale Before")
	}
	if _, err = os.Stat(f.intentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup retry failed", err)
	}
}

func TestGroupCommitCaptureCleanup(t *testing.T) {
	f, tree, _ := commitFixture(t)
	if err := os.Remove(f.intentPath()); err != nil {
		t.Fatal(err)
	}
	j := tree.intent
	g := groupIntent{Schema: storeSchema, StoreID: f.store.state.StoreID, TxID: [16]byte{4}, BaseGeneration: f.store.state.Generation, Generation: f.store.state.Generation + 1, Clock: f.clock, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration, Entries: []groupIntentEntry{{Target: j.Target, Tmp: j.Tmp, Capture: j.Capture, Before: j.Before, After: j.After, Identity: j.Identity}}}
	dev, ino, err := fileDevice(tree.incoming)
	if err != nil {
		t.Fatal(err)
	}
	g.Entries[0].IncomingIdentity = captureIdentity{dev, ino}
	var trace []string
	f.SetPersistenceProbe(PersistenceProbe{Phase: func(_, p string) { trace = append(trace, p) }, Flush: func(_, p string, _, _ uint64) {
		if p == "G3a" {
			if _, e := os.Stat(filepath.Join(f.Root, j.Target)); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("target installed before capture flush")
			}
		}
		if p == "G5" {
			for _, name := range []string{f.intentPath(), filepath.Join(f.Root, j.Capture)} {
				if _, e := os.Stat(name); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("cleanup order", name, e)
				}
			}
		}
	}})
	if err = runGroupCommit(f, g, []*commitTree{tree}, tree.dirs, func() {}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(trace, []string{"G1", "G2", "G3", "G4", "G5"}) {
		t.Fatal(trace)
	}
	b, err := os.ReadFile(filepath.Join(f.Root, j.Target))
	if err != nil || string(b) != "new" {
		t.Fatal(string(b), err)
	}
}
