package folder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func rootStateFixture(t *testing.T) *Folder {
	t.Helper()
	d := t.TempDir()
	f, err := Open(filepath.Join(d, "root"), filepath.Join(d, "index.json"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(f)
	t.Cleanup(func() { f.Close() })
	return f
}
func requireRootBlocked(t *testing.T, f *Folder) {
	t.Helper()
	if err := f.guardRoot(); err == nil || f.Blocked() == nil {
		t.Fatal("expected root latch")
	}
}
func TestLegacyNonemptyRequiresConfirmation(t *testing.T) {
	d := t.TempDir()
	state := filepath.Join(d, "index.json")
	data := []byte(`{"clock":7,"files":[{"name":"a","version":{"me":7}}]}`)
	if err := os.WriteFile(state, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(d, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(f)
	defer f.Close()
	if f.Blocked() == nil || f.clock != 7 || f.files["a"].Version["me"] != 7 {
		t.Fatal("migration lost state or trusted legacy")
	}
	archived, err := os.ReadFile(state + ".legacy")
	if err != nil || string(archived) != string(data) {
		t.Fatal("legacy archive missing", err)
	}
}
func TestMissingIndexedRootNotCreated(t *testing.T) {
	f := rootStateFixture(t)
	f.files["a"] = &FileInfo{Name: "a"}
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.Root); err != nil {
		t.Fatal(err)
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(g)
	defer g.Close()
	if g.Blocked() == nil {
		t.Fatal("missing latch")
	}
	if _, err := os.Lstat(f.Root); !os.IsNotExist(err) {
		t.Fatal("root recreated")
	}
}
func TestRootIdentityChange(t *testing.T) {
	for _, change := range []string{"symlink", "path", "device", "inode"} {
		t.Run(change, func(t *testing.T) {
			f := rootStateFixture(t)
			switch change {
			case "symlink", "inode":
				if err := os.Rename(f.Root, f.Root+"-old"); err != nil {
					t.Fatal(err)
				}
				if change == "symlink" {
					if err := os.Symlink(f.Root+"-old", f.Root); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(f.Root, 0755); err != nil {
					t.Fatal(err)
				}
			case "path":
				f.Root = t.TempDir()
			case "device":
				f.binding.Device++
			}
			requireRootBlocked(t, f)
		})
	}
}
func TestRootLatchSurvivesRestart(t *testing.T) {
	f := rootStateFixture(t)
	require := f.blockRoot("root_empty", "empty")
	if require == nil {
		t.Fatal("missing error")
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(g)
	defer g.Close()
	if g.Blocked() == nil || g.Blocked().Token != f.Blocked().Token {
		t.Fatal("latch changed on restart")
	}
}
func TestUnknownSchemaPreservesIndex(t *testing.T) {
	f := &Folder{Root: t.TempDir(), statePath: filepath.Join(t.TempDir(), "index.json")}
	data, _ := json.Marshal(persisted{Schema: 99})
	if err := os.WriteFile(f.statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.Root, f.statePath, "me", nil); err == nil {
		t.Fatal("accepted unknown schema")
	}
	got, err := os.ReadFile(f.statePath)
	if err != nil || string(got) != string(data) {
		t.Fatal("unknown schema overwritten")
	}
}

func TestConfirmedGenerationSurvivesRestart(t *testing.T) {
	f := rootStateFixture(t)
	f.confirmedGeneration = 3
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	g, err := Open(f.Root, f.statePath, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(g)
	defer g.Close()
	if g.ConfirmedGeneration() != 3 {
		t.Fatal("confirmed generation lost")
	}
}

func TestRootGuardFailureDoesNotChangeIndex(t *testing.T) {
	f := rootStateFixture(t)
	f.files["a"] = &FileInfo{Name: "a", Version: Vector{"me": 4}}
	f.clock = 4
	before := f.seq
	f.binding.Inode++
	requireRootBlocked(t, f)
	if f.files["a"].Deleted || f.clock != 4 || f.seq != before {
		t.Fatal("guard changed indexed state")
	}
}
