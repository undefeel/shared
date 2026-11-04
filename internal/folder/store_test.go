package folder

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreReplay(t *testing.T) {
	s := storeSnapshot{Schema: storeSchema}
	f := storeFrame{Generation: 1, Kind: "Commit", Clock: 1, Operations: []storeOperation{{Name: "a", After: &storeFile{Name: "a", Disk: "a"}}}}
	b, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	header := encodeLogHeader(storeLogHeader{})
	log := append(append([]byte{}, header...), b...)
	want, _, _, err := replayStore(bytes.NewReader(log), s)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(b); i++ {
		got, end, torn, err := replayStore(bytes.NewReader(log[:len(header)+i]), s)
		if err != nil || got.Generation != 0 || end != int64(len(header)) || torn != (i > 0) {
			t.Fatalf("tail %d: %v %d %v", i, err, end, torn)
		}
	}
	for i := 0; i < 2; i++ {
		got, _, _, err := replayStore(bytes.NewReader(log), want)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("repeated replay: %v", err)
		}
	}
	for _, kind := range []string{"crc-middle", "gap", "uuid", "before"} {
		t.Run(kind, func(t *testing.T) {
			bad := append([]byte{}, log...)
			switch kind {
			case "crc-middle":
				bad[len(bad)-1] ^= 1
				bad = append(bad, b...)
			case "gap":
				f.Generation = 3
				f.PreviousGeneration = 2
				b2, _ := encodeFrame(f)
				bad = append(bad, b2...)
			case "uuid":
				bad[12] = 1
			case "before":
				f.Generation = 2
				f.PreviousGeneration = 1
				f.Operations[0].Before = [32]byte{}
				b2, _ := encodeFrame(f)
				bad = append(bad, b2...)
			}
			if _, _, _, err := replayStore(bytes.NewReader(bad), s); err == nil {
				t.Fatal("accepted corrupt log")
			}
		})
	}
}
func TestStoreAppendDurability(t *testing.T) {
	for _, fault := range []string{"short", "flush", "ok"} {
		t.Run(fault, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.log")
			if err := os.WriteFile(path, encodeLogHeader(storeLogHeader{}), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := openIndexStore(path, storeSnapshot{Schema: storeSchema})
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			calls := 0
			s.flush = func(f *os.File) error {
				calls++
				if fault == "flush" {
					return io.ErrClosedPipe
				}
				return syncFile(f)
			}
			if fault == "short" {
				s.write = func(b []byte) (int, error) { return s.log.Write(b[:len(b)/2]) }
			}
			frame := storeFrame{Kind: "Control", Generation: 1, Control: &storeControl{ConfirmedGeneration: 7}}
			err = s.append(frame)
			if fault != "ok" {
				if err == nil || s.state.Generation != 0 {
					t.Fatal("failed durability published")
				}
				if fault == "short" && (!errors.Is(err, io.ErrShortWrite) || calls != 0) {
					t.Fatal("short write not checked")
				}
				if s.append(frame) == nil {
					t.Fatal("failed writer reused")
				}
			} else if err != nil || calls != 1 || s.state.Control.ConfirmedGeneration != 7 {
				t.Fatalf("append: %v", err)
			}
			// Restart resolves a complete frame even when the caller saw a flush error.
			reopened, err := openIndexStore(path, storeSnapshot{Schema: storeSchema})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			if fault == "short" {
				st, _ := os.Stat(path)
				if st.Size() != logHeaderSize {
					t.Fatal("tail not truncated")
				}
				if err := reopened.append(frame); err != nil {
					t.Fatal(err)
				}
			} else if reopened.state.Generation != 1 {
				t.Fatal("lost complete frame")
			}
		})
	}
}
