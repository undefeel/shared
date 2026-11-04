package folder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGroupRecovery(t *testing.T) {
	for _, phase := range []int{0, 1, 2, 3} {
		t.Run(string(rune('0'+phase)), func(t *testing.T) {
			f, j := journalFixture(t, phase)
			j.Before.Size = 3
			j.After.Size = 3
			g := groupIntent{Schema: storeSchema, StoreID: [16]byte{1}, TxID: [16]byte{2}, BaseGeneration: j.Generation - 1, Generation: j.Generation, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration,
				Entries: []groupIntentEntry{{Target: j.Target, Capture: j.Capture, Before: j.Before, After: j.After, Identity: j.Identity, Conflict: "a.sync-conflict-recovery"}}}
			state := storeSnapshot{Schema: storeSchema, StoreID: g.StoreID, Generation: g.BaseGeneration, Files: []storeFile{*recoveryFile(j.Before)}, Control: storeControl{Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration}}
			if phase != 3 {
				untouched := *j.Before
				untouched.Name = "b"
				if err := os.WriteFile(filepath.Join(f.Root, "b"), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				g.Entries = append(g.Entries, groupIntentEntry{Target: "b", Before: &untouched, After: &untouched})
				state.Files = append(state.Files, *recoveryFile(&untouched))
			}
			var frame *storeFrame
			if phase == 3 {
				state.Generation = g.Generation
				state.Files = []storeFile{*recoveryFile(j.After)}
				frame = &storeFrame{Kind: "Commit", TxID: g.TxID, Generation: g.Generation, PreviousGeneration: g.BaseGeneration, Operations: []storeOperation{{Name: "a", Before: storeDigest(*recoveryFile(j.Before)), After: recoveryFile(j.After)}}}
			}
			if err := f.writeGroupRecovery(g); err != nil {
				t.Fatal(err)
			}
			for retry := 0; retry < 2; retry++ {
				if err := f.recoverGroup(g, state, frame); err != nil {
					t.Fatal(err)
				}
			}
			want := "old"
			if phase >= 2 {
				want = "new"
			}
			b, err := os.ReadFile(filepath.Join(f.Root, "a"))
			if err != nil || string(b) != want {
				t.Fatalf("target %q %v", b, err)
			}
			if phase == 2 {
				b, err = os.ReadFile(filepath.Join(f.Root, g.Entries[0].Conflict))
				if err != nil || string(b) != "old" {
					t.Fatalf("conflict %q %v", b, err)
				}
			}
			if _, err = os.Stat(f.intentPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("intent: %v", err)
			}
		})
	}
}

func TestGroupRecoveryChecksAllBeforeMutation(t *testing.T) {
	f, j := journalFixture(t, 1)
	j.Before.Size = 3
	j.After.Size = 3
	g := groupIntent{Schema: storeSchema, StoreID: [16]byte{1}, TxID: [16]byte{2}, BaseGeneration: j.Generation - 1, Generation: j.Generation, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration,
		Entries: []groupIntentEntry{{Target: j.Target, Capture: j.Capture, Before: j.Before, After: j.After, Identity: j.Identity}, {Target: "b", After: &FileInfo{Name: "b"}}}}
	state := storeSnapshot{StoreID: g.StoreID, Generation: g.BaseGeneration, Files: []storeFile{*recoveryFile(j.Before), {Name: "b", Hash: "unexpected"}}, Control: storeControl{Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration}}
	if err := f.recoverGroup(g, state, nil); err == nil {
		t.Fatal("accepted mismatched group")
	}
	if _, err := os.Stat(filepath.Join(f.Root, j.Capture)); err != nil {
		t.Fatal("capture changed before validation", err)
	}
}

func TestGroupRecoveryMixedStatesAndInterruptedRetry(t *testing.T) {
	const conflict = "b.sync-conflict-recovery"
	newFixture := func(t *testing.T) (*Folder, groupIntent, storeSnapshot) {
		t.Helper()
		f, j := journalFixture(t, 1) // a is captured, and its target is not installed.
		j.Before.Size = 3
		j.After.Size = 3
		hash := func(s string) string {
			digest := sha256.Sum256([]byte(s))
			return hex.EncodeToString(digest[:])
		}
		before := func(name, body string) *FileInfo {
			return &FileInfo{Name: name, Hash: hash(body), Size: int64(len(body)), Version: Vector{"local": 1}}
		}
		after := func(name, body string) *FileInfo {
			return &FileInfo{Name: name, Hash: hash(body), Size: int64(len(body)), Version: Vector{"peer": 1}}
		}

		// b is captured, then a target is installed before recovery. Recovery
		// must preserve that target and move the captured original to a conflict.
		bBefore, bAfter := before("b", "old-b"), after("b", "new-b")
		if err := os.WriteFile(filepath.Join(f.Root, "b"), []byte("old-b"), 0600); err != nil {
			t.Fatal(err)
		}
		bCapture, err := f.root.prepareCapture("b")
		if err != nil {
			t.Fatal(err)
		}
		var bStat captureIdentity
		bf, err := f.root.openRead("b")
		if err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		err = unix.Fstat(int(bf.Fd()), &st)
		bf.Close()
		if err != nil {
			bCapture.Close()
			t.Fatal(err)
		}
		bStat = captureIdentity{uint64(st.Dev), uint64(st.Ino)}
		if err = bCapture.take(); err != nil {
			bCapture.Close()
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(f.Root, "b"), []byte("new-b"), 0600); err != nil {
			bCapture.Close()
			t.Fatal(err)
		}
		bCapture.Close()

		// c has not been touched: its original target remains and no capture
		// was staged for it.
		cBefore, cAfter := before("c", "old-c"), after("c", "new-c")
		if err = os.WriteFile(filepath.Join(f.Root, "c"), []byte("old-c"), 0600); err != nil {
			t.Fatal(err)
		}
		g := groupIntent{Schema: storeSchema, StoreID: [16]byte{1}, TxID: [16]byte{3}, BaseGeneration: j.Generation - 1, Generation: j.Generation, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration,
			Entries: []groupIntentEntry{
				{Target: "a", Capture: j.Capture, Before: j.Before, After: j.After, Identity: j.Identity},
				{Target: "b", Capture: bCapture.Recovery, Before: bBefore, After: bAfter, Identity: bStat, Conflict: conflict},
				{Target: "c", Before: cBefore, After: cAfter},
			}}
		state := storeSnapshot{Schema: storeSchema, StoreID: g.StoreID, Generation: g.BaseGeneration,
			Files:   []storeFile{*recoveryFile(j.Before), *recoveryFile(bBefore), *recoveryFile(cBefore)},
			Control: storeControl{Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration}}
		if err = f.writeGroupRecovery(g); err != nil {
			t.Fatal(err)
		}
		return f, g, state
	}
	rootFiles := func(t *testing.T, f *Folder) map[string]string {
		t.Helper()
		entries, err := os.ReadDir(f.Root)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string, len(entries))
		for _, entry := range entries {
			b, err := os.ReadFile(filepath.Join(f.Root, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			got[entry.Name()] = string(b)
		}
		return got
	}

	baseline, baselineGroup, baselineState := newFixture(t)
	if err := baseline.recoverGroup(baselineGroup, baselineState, nil); err != nil {
		t.Fatal("uninterrupted recovery:", err)
	}
	want := rootFiles(t, baseline)

	interrupted, interruptedGroup, interruptedState := newFixture(t)
	boom := errors.New("injected recovery interruption after first entry")
	flushes := 0
	interrupted.persistHooks.syncDir = func(dir *os.File) error {
		if err := syncDir(dir); err != nil {
			return err
		}
		flushes++
		if flushes == 1 {
			return boom
		}
		return nil
	}
	if err := interrupted.recoverGroup(interruptedGroup, interruptedState, nil); !errors.Is(err, boom) {
		t.Fatalf("expected injected interruption, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(interrupted.Root, "a")); err != nil || string(got) != "old" {
		t.Fatalf("first entry was not durably restored before interruption: %q %v", got, err)
	}
	interrupted.persistHooks.syncDir = nil
	if err := interrupted.recoverGroup(interruptedGroup, interruptedState, nil); err != nil {
		t.Fatal("retry recovery:", err)
	}
	if err := interrupted.recoverGroup(interruptedGroup, interruptedState, nil); err != nil {
		t.Fatal("idempotent recovery:", err)
	}
	got := rootFiles(t, interrupted)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interrupted recovery differs from uninterrupted recovery:\n got: %#v\nwant: %#v", got, want)
	}
	for name, body := range map[string]string{"a": "old", "b": "new-b", "c": "old-c", conflict: "old-b"} {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
		}
	}
	if _, err := os.Stat(interrupted.intentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains after retry: %v", err)
	}
}
