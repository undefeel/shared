package folder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
