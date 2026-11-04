package folder

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func compactFixture(t *testing.T) (*indexStore, storeSnapshot, []byte) {
	t.Helper()
	base := storeSnapshot{Schema: storeSchema, StoreID: [16]byte{1}}
	dir := t.TempDir()
	b, err := encodeSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "index.snapshot"), b, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index.log")
	if err = os.WriteFile(path, encodeLogHeader(storeLogHeader{StoreID: base.StoreID}), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openIndexStore(path, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	for i := uint64(1); i <= 3; i++ {
		c := storeControl{ConfirmedGeneration: i, PeerSetEpoch: 9, GCFloor: []storePair{{Peer: "p", Counter: i}}}
		f := storeFile{Name: "a", Disk: "a", Version: []storePair{{Peer: "p", Counter: i}}}
		var before [32]byte
		if i > 1 {
			before = storeDigest(s.state.Files[0])
		}
		if err = s.append(storeFrame{Kind: "Commit", PreviousGeneration: i - 1, Generation: i, Clock: i, Control: &c, Operations: []storeOperation{{Name: "a", Before: before, After: &f}}}); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, base, old
}

func TestCompactionCrashBoundaries(t *testing.T) {
	for _, stop := range []string{"snapshot-flush", "snapshot-rename", "log-flush", "log-rename", "ok"} {
		t.Run(stop, func(t *testing.T) {
			s, base, old := compactFixture(t)
			want := s.state
			dir := filepath.Dir(s.log.Name())
			flushes, renames := 0, 0
			fault := errors.New("power cut")
			s.flush = func(f *os.File) error {
				flushes++
				if (stop == "snapshot-flush" && flushes == 1) || (stop == "log-flush" && flushes == 2) {
					return fault
				}
				return syncFile(f)
			}
			err := s.compactWithSyncDir(func(d *os.File) error {
				renames++
				if renames == 1 {
					b, e := os.ReadFile(filepath.Join(dir, "index.log"))
					if e != nil || !bytes.Equal(b, old) {
						t.Fatal("old log modified before durable snapshot", e)
					}
				}
				if (stop == "snapshot-rename" && renames == 1) || (stop == "log-rename" && renames == 2) {
					return fault
				}
				return syncDir(d)
			})
			if stop == "ok" {
				if err != nil || flushes != 2 || renames != 2 {
					t.Fatalf("%v flush=%d rename=%d", err, flushes, renames)
				}
			} else {
				if !errors.Is(err, fault) {
					t.Fatalf("lost failure: %v", err)
				}
				if s.append(storeFrame{Kind: "Control", PreviousGeneration: 3, Generation: 4}) == nil {
					t.Fatal("writer not fenced")
				}
			}
			b, e := os.ReadFile(filepath.Join(dir, "index.snapshot"))
			if e != nil {
				t.Fatal(e)
			}
			snap, e := decodeSnapshot(bytes.NewReader(b))
			if e != nil {
				t.Fatal(e)
			}
			if stop == "snapshot-flush" && !reflect.DeepEqual(snap, base) {
				t.Fatal("snapshot prematurely replaced")
			}
			r, e := openIndexStore(filepath.Join(dir, "index.log"), snap)
			if e != nil {
				t.Fatal(e)
			}
			defer r.close()
			if !reflect.DeepEqual(r.state, want) {
				t.Fatal("restart lost state")
			}
			writer := r
			if stop == "ok" {
				writer = s
			}
			if e = writer.append(storeFrame{Kind: "Control", PreviousGeneration: 3, Generation: 4, Clock: 3}); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestCompactionFullOldLog(t *testing.T) {
	s, _, old := compactFixture(t)
	snap := s.state
	if err := s.compact(); err != nil {
		t.Fatal(err)
	}
	got, _, torn, err := replayStore(bytes.NewReader(old), snap)
	if err != nil || torn || !reflect.DeepEqual(got, snap) {
		t.Fatalf("full old log: %v", err)
	}
	old[len(old)-1] ^= 1
	if _, _, _, err = replayStore(bytes.NewReader(old), snap); err == nil {
		t.Fatal("superseded corrupt frame accepted")
	}
	if needed, err := s.needsCompaction(0, 0); err != nil || needed {
		t.Fatalf("idle rewrites snapshot: %v", err)
	}
}

func TestCompactionThresholdAndIntent(t *testing.T) {
	s, _, old := compactFixture(t)
	for _, tc := range []struct {
		removed, total uint64
		want           bool
	}{{0, 0, false}, {1, 5, false}, {1, 4, true}, {2, 5, true}} {
		got, err := s.needsCompaction(tc.removed, tc.total)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %v %v", tc, got, err)
		}
	}
	dir := filepath.Dir(s.log.Name())
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.compact() == nil {
		t.Fatal("compacted with pending intent")
	}
	b, err := os.ReadFile(s.log.Name())
	if err != nil || !bytes.Equal(b, old) {
		t.Fatal("log changed with pending intent")
	}
}
