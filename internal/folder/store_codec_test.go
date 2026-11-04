package folder

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"io"
	"reflect"
	"strings"
	"testing"
)

func codecFixture() storeSnapshot {
	return storeSnapshot{Schema: 3, StoreID: [16]byte{1}, Generation: 7, Clock: 9,
		Files:   []storeFile{{Name: "é.txt", Disk: "e\u0301.txt", Size: 42, Version: []storePair{{"a", 9}, {"b", 2}}}},
		Control: storeControl{Binding: rootBinding{"/root", 12, 34}, Blocked: &RootError{"root_changed", "stop", "secret"}, ConfirmedGeneration: 4, PeerSetEpoch: 2, Migration: true, ACK: []storeAck{{"b", "é.txt", []storePair{{"a", 9}}}}, GCFloor: []storePair{{"a", 1}}}}
}
func TestStoreCodecRoundtrip(t *testing.T) {
	s := codecFixture()
	for _, compressed := range []bool{false, true} {
		if compressed {
			s.Control.Blocked.Message = strings.Repeat("blocked", 2000)
		}
		b, err := encodeSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		if compressed && b[12] != 1 {
			t.Fatal("expected gzip")
		}
		got, err := decodeSnapshot(bytes.NewReader(b))
		if err != nil || !reflect.DeepEqual(s, got) {
			t.Fatalf("roundtrip: %v %+v", err, got)
		}
		again, _ := encodeSnapshot(s)
		if !bytes.Equal(b, again) {
			t.Fatal("nondeterministic snapshot")
		}
	}
	h := storeLogHeader{s.StoreID, s.Generation}
	got, err := decodeLogHeader(bytes.NewReader(encodeLogHeader(h)))
	if err != nil || got != h {
		t.Fatal(got, err)
	}
	f := storeFrame{Generation: 8, PreviousGeneration: 7, TxID: [16]byte{4}, Kind: "Commit", Clock: 10, Control: &s.Control, Operations: []storeOperation{{Name: s.Files[0].Name, After: &s.Files[0]}}}
	b, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	stream := bytes.NewReader(append(append([]byte{}, b...), b...))
	for range 2 {
		got, err := decodeFrame(stream)
		if err != nil || !reflect.DeepEqual(f, got) {
			t.Fatal(got, err)
		}
	}
	if _, err = decodeFrame(stream); err != io.EOF {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if _, err = decodeFrame(bytes.NewReader(b)); err == nil {
		t.Fatal("CRC corruption accepted")
	}
	for _, n := range []int{1, frameHeaderSize - 1, frameHeaderSize + 1} {
		if _, err = decodeFrame(bytes.NewReader(b[:n])); err != io.ErrUnexpectedEOF {
			t.Fatalf("torn tail: %v", err)
		}
	}
}
func TestStoreCodecValidation(t *testing.T) {
	for _, mutate := range []func(*storeSnapshot){
		func(s *storeSnapshot) { s.Files = append(s.Files, s.Files[0]) },
		func(s *storeSnapshot) { s.Files[0].Name = "../bad" },
		func(s *storeSnapshot) { s.Files[0].Disk = "other" },
		func(s *storeSnapshot) { s.Files[0].Size = -1 },
		func(s *storeSnapshot) { s.Files[0].Version[1].Peer = "a" },
		func(s *storeSnapshot) { s.Files[0].Version[0].Counter = 0 },
	} {
		s := codecFixture()
		mutate(&s)
		if _, err := encodeSnapshot(s); err == nil {
			t.Fatal("invalid encode accepted")
		}
		// Bypass encoder validation to exercise hostile on-disk input.
		var p bytes.Buffer
		if err := gob.NewEncoder(&p).Encode(s); err != nil {
			t.Fatal(err)
		}
		b := rawSnapshot(p.Bytes())
		if _, err := decodeSnapshot(bytes.NewReader(b)); err == nil {
			t.Fatal("invalid decode accepted")
		}
	}
}
func rawSnapshot(p []byte) []byte {
	b := make([]byte, snapshotHeaderSize)
	copy(b, "SHDSNP03")
	binary.BigEndian.PutUint32(b[8:], 3)
	binary.BigEndian.PutUint64(b[13:], uint64(len(p)))
	binary.BigEndian.PutUint64(b[21:], uint64(len(p)))
	sum := sha256.Sum256(p)
	copy(b[29:], sum[:])
	return append(b, p...)
}
func TestStoreCodecBoundsAndTrailing(t *testing.T) {
	b, _ := encodeSnapshot(codecFixture())
	for _, off := range []int{13, 21} {
		bad := append([]byte{}, b...)
		binary.BigEndian.PutUint64(bad[off:], ^uint64(0))
		if _, err := decodeSnapshot(bytes.NewReader(bad[:snapshotHeaderSize])); err == nil {
			t.Fatal("oversized snapshot")
		}
	}
	h := make([]byte, frameHeaderSize)
	binary.BigEndian.PutUint32(h, frameLimit+1)
	if _, err := decodeFrame(bytes.NewReader(h)); err == nil {
		t.Fatal("oversized frame")
	}
	if _, err := decodeSnapshot(bytes.NewReader(append(b, 1))); err == nil {
		t.Fatal("snapshot trailing bytes")
	}
	var p bytes.Buffer
	e := gob.NewEncoder(&p)
	_ = e.Encode(codecFixture())
	_ = e.Encode(42)
	if _, err := decodeSnapshot(bytes.NewReader(rawSnapshot(p.Bytes()))); err == nil {
		t.Fatal("second gob accepted")
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write(bytes.Repeat([]byte{0}, 1<<20))
	_ = w.Close()
	var out storeSnapshot
	if err := decodeStorePayload(z.Bytes(), 16, 1, snapshotLimit, &out); err == nil {
		t.Fatal("gzip bomb accepted")
	}
	bomb := rawSnapshot(make([]byte, 16))[:snapshotHeaderSize]
	bomb[12] = 1
	binary.BigEndian.PutUint64(bomb[13:], uint64(z.Len()))
	if _, err := decodeSnapshot(bytes.NewReader(append(bomb, z.Bytes()...))); err == nil {
		t.Fatal("snapshot gzip bomb")
	}
	z.Reset()
	w = gzip.NewWriter(&z)
	_, _ = w.Write(p.Bytes())
	_ = w.Close()
	if err := decodeStorePayload(append(z.Bytes(), 0), uint64(p.Len()), 1, snapshotLimit, &out); err == nil {
		t.Fatal("gzip trailing bytes")
	}
}
