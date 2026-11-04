package folder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationRestart(t *testing.T) {
	for _, schema := range []int{0, 2} {
		for _, stop := range []int{0, 1, 2, 3, 4} {
			t.Run(string(rune('0'+schema))+string(rune('0'+stop)), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "index.json")
				p := persisted{Schema: schema, Generation: 7, Clock: 9, Files: []*FileInfo{{Name: "gone", Deleted: true, Version: Vector{"peer": 9}}}}
				if schema == 2 {
					p.Binding = rootBinding{Path: "/root", Device: 1, Inode: 2}
					p.Blocked = &RootError{"root_empty", "empty", "token"}
					p.ConfirmedGeneration = 6
				}
				data, _ := json.Marshal(p)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				calls, prepared := 0, 0
				flush := func(f *os.File) error {
					calls++
					if calls == stop {
						return errors.New("crash")
					}
					return syncFile(f)
				}
				prepare := func() error { prepared++; return nil }
				s, err := migrateStore(path, prepare, flush, syncDir)
				if stop > 0 && err == nil {
					t.Fatal("durability error reported success")
				}
				if s != nil {
					s.close()
				}
				// Once snapshot exists, obsolete JSON (even invalid) must never be read.
				_, e := os.Stat(filepath.Join(filepath.Dir(path), "index.snapshot"))
				switched := e == nil
				if switched {
					os.WriteFile(path, []byte("obsolete"), 0600)
				}
				before := prepared
				s, err = migrateIndexStore(path, prepare)
				if err != nil {
					t.Fatal(err)
				}
				defer s.close()
				if switched && prepared != before {
					t.Fatal("legacy recovery after switch")
				}
				if s.state.Control.Migration || s.state.Clock != 9 || s.state.Files[0].DeletedAt <= 0 || len(s.state.Control.ACK) != 0 {
					t.Fatal(s.state)
				}
				if s.state.Control.Blocked == nil || s.state.Control.Blocked.Token == "" {
					t.Fatal("lost latch")
				}
				if schema == 2 && (s.state.Control.Blocked.Token != "token" || s.state.Control.Binding != p.Binding || s.state.Control.ConfirmedGeneration != 6) {
					t.Fatal("lost control state")
				}
			})
		}
	}
}

func TestMigrationLegacyRecoveryAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	os.WriteFile(path, []byte(`{"schema":0,"files":[{"name":"a","version":{"p":1}}]}`), 0600)
	failure := errors.New("legacy intent unresolved")
	if _, err := migrateIndexStore(path, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "index.snapshot")); !os.IsNotExist(err) {
		t.Fatal("switched before recovery")
	}
	// Model finishNewIntent durably replacing the old JSON before migration.
	s, err := migrateIndexStore(path, func() error {
		return os.WriteFile(path, []byte(`{"schema":2,"generation":8,"clock":12,"files":[{"name":"finished","version":{"p":12}}]}`), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.state.Files[0].Name != "finished" || s.state.Clock != 12 || s.state.Generation != 9 {
		t.Fatal("did not migrate recovered JSON")
	}
	for _, data := range []string{`{"schema":1}`, `{"files":[null]}`, `{"files":[{"name":"a"},{"name":"a"}]}`, `{"files":[{"name":"a","version":{"p":0}}]}`} {
		if _, err := legacySnapshot([]byte(data), time.Now()); err == nil {
			t.Fatal(data)
		}
	}
}
