package folder

import (
	"encoding/json"
	"reflect"
	"testing"
)

func groupIntentFixture() groupIntent {
	return groupIntent{Schema: storeSchema, StoreID: [16]byte{1}, TxID: [16]byte{2},
		BaseGeneration: 7, Generation: 8, ConfirmedGeneration: 3,
		Binding: rootBinding{Path: "/root", Device: 1, Inode: 2},
		Entries: []groupIntentEntry{{Target: "file", Tmp: tmpPrefix + "incoming", Capture: tmpPrefix + "capture-old", Conflict: "file.sync-conflict-test",
			Before:   &FileInfo{Name: "file", Hash: "old", Version: Vector{"peer": 1}},
			After:    &FileInfo{Name: "file", Hash: "new", Version: Vector{"peer": 2}},
			Identity: captureIdentity{1, 3}, IncomingIdentity: captureIdentity{1, 4}}}}
}

func TestGroupIntentJSONRoundtrip(t *testing.T) {
	g := groupIntentFixture()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var got groupIntent
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err = got.validate(g.Binding, g.ConfirmedGeneration); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, got) {
		t.Fatal("recovery fields lost")
	}
}

func TestGroupIntentRejectsAmbiguity(t *testing.T) {
	tests := map[string]func(*groupIntent){
		"duplicate target": func(g *groupIntent) { g.Entries = append(g.Entries, g.Entries[0]) },
		"invalid Disk":     func(g *groupIntent) { g.Entries[0].After.Disk = "../file" },
		"reserved conflict": func(g *groupIntent) {
			g.Entries = append(g.Entries, groupIntentEntry{Target: g.Entries[0].Conflict, After: &FileInfo{Name: g.Entries[0].Conflict}})
		},
		"binding mismatch":   func(g *groupIntent) { g.Binding.Inode++ },
		"confirmed mismatch": func(g *groupIntent) { g.ConfirmedGeneration++ },
		"generation gap":     func(g *groupIntent) { g.Generation++ },
		"missing identity":   func(g *groupIntent) { g.Entries[0].IncomingIdentity.Inode = 0 },
		"parent child": func(g *groupIntent) {
			g.Entries = append(g.Entries, groupIntentEntry{Target: "file/child", After: &FileInfo{Name: "file/child"}})
		},
		"capture overlap": func(g *groupIntent) { g.Entries[0].Conflict = g.Entries[0].Capture },
		"invalid parent":  func(g *groupIntent) { g.CreatedParents = []string{"../outside"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			g := groupIntentFixture()
			binding, confirmed := g.Binding, g.ConfirmedGeneration
			mutate(&g)
			if err := g.validate(binding, confirmed); err == nil {
				t.Fatal("accepted ambiguous intent")
			}
		})
	}
}

func TestGroupIntentLocalNFD(t *testing.T) {
	g := groupIntentFixture()
	g.Entries = []groupIntentEntry{{Target: "e\u0301", After: &FileInfo{Name: "é", Disk: "e\u0301"}}}
	if err := g.validate(g.Binding, g.ConfirmedGeneration); err != nil {
		t.Fatal(err)
	}
	g.Entries = append(g.Entries, groupIntentEntry{Target: "é", After: &FileInfo{Name: "é"}})
	if err := g.validate(g.Binding, g.ConfirmedGeneration); err == nil {
		t.Fatal("accepted canonical duplicate")
	}
}
