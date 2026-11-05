package folder

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	storeSchema        = 3
	snapshotLimit      = 256 << 20
	frameLimit         = 16 << 20
	snapshotHeaderSize = 61
	logHeaderSize      = 36
	frameHeaderSize    = 37
)

// These concrete wire types deliberately contain no maps or interfaces.
// Callers supply sorted lists; decoding rejects noncanonical ordering.
type storePair struct {
	Peer    string
	Counter uint64
}
type storeFile struct {
	Name, Disk, Hash         string
	Size, ModTime, DeletedAt int64
	Exec, Deleted            bool
	Version                  []storePair
}
type storeAck struct {
	Peer, Name string
	Version    []storePair
}
type storeMember struct {
	ID, RemoteEpoch, Challenge string
	Baseline                   bool
	Checkpoint                 uint64
}
type storeRetired struct {
	Name    string
	Version []storePair
}
type storeControl struct {
	Binding                           rootBinding
	Blocked                           *RootError
	ConfirmedGeneration, PeerSetEpoch uint64
	ACK                               []storeAck
	GCFloor                           []storePair
	Members                           []storeMember
	Retired                           []storeRetired
	Migration                         bool
}
type storeSnapshot struct {
	Schema            uint32
	StoreID           [16]byte
	Generation, Clock uint64
	Files             []storeFile
	Control           storeControl
}
type storeOperation struct {
	Name         string
	Before       [32]byte
	After        *storeFile
	DeleteRecord bool // GC removal, distinct from After.Deleted.
}
type storeFrame struct {
	Generation         uint64 // envelope, checked by replay against previous durable generation
	TxID               [16]byte
	PreviousGeneration uint64
	Kind               string // Commit, Control, PeerAck, GC
	Operations         []storeOperation
	Clock              uint64
	Control            *storeControl
}
type storeLogHeader struct {
	StoreID        [16]byte
	BaseGeneration uint64
}

func codecError() error { return fmt.Errorf("invalid schema=3 store encoding") }
func validatePairs(v []storePair) error {
	for i, p := range v {
		if p.Peer == "" || p.Counter == 0 || (i > 0 && v[i-1].Peer >= p.Peer) {
			return codecError()
		}
	}
	return nil
}
func validateStoreFile(f storeFile) error {
	if !validDisk(f.Name, f.Disk) || f.Size < 0 || f.DeletedAt < 0 {
		return codecError()
	}
	return validatePairs(f.Version)
}
func validateControl(c storeControl) error {
	for i, m := range c.Members {
		if m.ID == "" || (i > 0 && c.Members[i-1].ID >= m.ID) || (m.Baseline && (m.RemoteEpoch == "" || m.Challenge == "")) {
			return codecError()
		}
	}
	for i, r := range c.Retired {
		if !validName(r.Name) || len(r.Version) == 0 || (i > 0 && !retiredLess(c.Retired[i-1], r)) {
			return codecError()
		}
		if err := validatePairs(r.Version); err != nil {
			return err
		}
	}
	if err := validatePairs(c.GCFloor); err != nil {
		return err
	}
	for i, a := range c.ACK {
		if a.Peer == "" || !validName(a.Name) {
			return codecError()
		}
		if i > 0 {
			p := c.ACK[i-1]
			if p.Peer > a.Peer || (p.Peer == a.Peer && p.Name >= a.Name) {
				return codecError()
			}
		}
		if err := validatePairs(a.Version); err != nil {
			return err
		}
	}
	return nil
}
func (s storeSnapshot) validate() error {
	if s.Schema != storeSchema {
		return codecError()
	}
	for i, f := range s.Files {
		if i > 0 && s.Files[i-1].Name >= f.Name {
			return codecError()
		}
		if err := validateStoreFile(f); err != nil {
			return err
		}
	}
	return validateControl(s.Control)
}
func (f storeFrame) validate() error {
	if f.PreviousGeneration == ^uint64(0) || f.Generation != f.PreviousGeneration+1 {
		return codecError()
	}
	switch f.Kind {
	case "Commit", "Control", "PeerAck", "GC":
	default:
		return codecError()
	}
	for i, op := range f.Operations {
		if !validName(op.Name) || (i > 0 && f.Operations[i-1].Name >= op.Name) || op.DeleteRecord == (op.After != nil) {
			return codecError()
		}
		if op.After != nil {
			if op.After.Name != op.Name {
				return codecError()
			}
			if err := validateStoreFile(*op.After); err != nil {
				return err
			}
		}
	}
	if f.Control != nil {
		return validateControl(*f.Control)
	}
	return nil
}

// Each invocation starts a new gob dictionary. Limits apply before compression.
func encodeStorePayload(v any, limit int) (plain, stored []byte, codec byte, err error) {
	var b bytes.Buffer
	err = gob.NewEncoder(&b).Encode(v)
	if err != nil {
		return
	}
	plain = b.Bytes()
	if len(plain) > limit {
		err = fmt.Errorf("store payload exceeds %d bytes", limit)
		return
	}
	stored = plain
	if len(plain) >= 4096 {
		var z bytes.Buffer
		w, e := gzip.NewWriterLevel(&z, gzip.BestSpeed)
		if e != nil {
			err = e
			return
		}
		if _, err = w.Write(plain); err != nil {
			return
		}
		if err = w.Close(); err != nil {
			return
		}
		if z.Len()*8 <= len(plain)*7 {
			stored, codec = z.Bytes(), 1
		}
	}
	return
}
func decodeStorePayload(stored []byte, plainLen uint64, codec byte, limit int, v any) error {
	if plainLen > uint64(limit) || len(stored) > limit {
		return codecError()
	}
	plain := stored
	switch codec {
	case 0:
		if uint64(len(stored)) != plainLen {
			return codecError()
		}
	case 1:
		r := bytes.NewReader(stored)
		z, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		z.Multistream(false)
		plain, err = io.ReadAll(io.LimitReader(z, int64(plainLen)+1))
		closeErr := z.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if uint64(len(plain)) != plainLen || r.Len() != 0 {
			return codecError()
		}
	default:
		return codecError()
	}
	r := bytes.NewReader(plain)
	d := gob.NewDecoder(r)
	if err := d.Decode(v); err != nil {
		return err
	}
	// A second value (even a type-incompatible value) or stray bytes is forbidden.
	var extra struct{}
	if err := d.Decode(&extra); err != io.EOF {
		return codecError()
	}
	return nil
}
func encodeSnapshot(s storeSnapshot) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	plain, stored, codec, err := encodeStorePayload(s, snapshotLimit)
	if err != nil {
		return nil, err
	}
	h := make([]byte, snapshotHeaderSize)
	copy(h, "SHDSNP03")
	binary.BigEndian.PutUint32(h[8:], storeSchema)
	h[12] = codec
	binary.BigEndian.PutUint64(h[13:], uint64(len(stored)))
	binary.BigEndian.PutUint64(h[21:], uint64(len(plain)))
	sum := sha256.Sum256(plain)
	copy(h[29:], sum[:])
	return append(h, stored...), nil
}
func decodeSnapshot(r io.Reader) (s storeSnapshot, err error) {
	h := make([]byte, snapshotHeaderSize)
	if _, err = io.ReadFull(r, h); err != nil {
		return
	}
	n, p := binary.BigEndian.Uint64(h[13:]), binary.BigEndian.Uint64(h[21:])
	if string(h[:8]) != "SHDSNP03" || binary.BigEndian.Uint32(h[8:]) != storeSchema || n > snapshotLimit || p > snapshotLimit {
		return s, codecError()
	}
	b := make([]byte, int(n))
	if _, err = io.ReadFull(r, b); err != nil {
		return
	}
	// Verify the digest of exactly the decompressed payload, before gob decoding.
	var plain []byte
	if h[12] == 0 {
		plain = b
	} else if h[12] == 1 {
		br := bytes.NewReader(b)
		z, e := gzip.NewReader(br)
		if e != nil {
			return s, e
		}
		z.Multistream(false)
		plain, err = io.ReadAll(io.LimitReader(z, int64(p)+1))
		ce := z.Close()
		if err != nil {
			return
		}
		if ce != nil {
			return s, ce
		}
		if br.Len() != 0 {
			return s, codecError()
		}
	} else {
		return s, codecError()
	}
	if uint64(len(plain)) != p || sha256.Sum256(plain) != [32]byte(h[29:61]) {
		return s, codecError()
	}
	if err = decodeStorePayload(plain, p, 0, snapshotLimit, &s); err != nil {
		return storeSnapshot{}, err
	}
	var tail [1]byte
	if n, e := io.ReadFull(r, tail[:]); n != 0 || e != io.EOF {
		return storeSnapshot{}, codecError()
	}
	if err = s.validate(); err != nil {
		return storeSnapshot{}, err
	}
	return
}
func encodeLogHeader(h storeLogHeader) []byte {
	b := make([]byte, logHeaderSize)
	copy(b, "SHDLOG03")
	binary.BigEndian.PutUint32(b[8:], storeSchema)
	copy(b[12:], h.StoreID[:])
	binary.BigEndian.PutUint64(b[28:], h.BaseGeneration)
	return b
}
func decodeLogHeader(r io.Reader) (h storeLogHeader, err error) {
	b := make([]byte, logHeaderSize)
	if _, err = io.ReadFull(r, b); err != nil {
		return
	}
	if string(b[:8]) != "SHDLOG03" || binary.BigEndian.Uint32(b[8:]) != storeSchema {
		return h, codecError()
	}
	copy(h.StoreID[:], b[12:28])
	h.BaseGeneration = binary.BigEndian.Uint64(b[28:])
	return
}

var storeCRC = crc32.MakeTable(crc32.Castagnoli)

func encodeFrame(f storeFrame) ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	plain, stored, codec, err := encodeStorePayload(f, frameLimit)
	if err != nil {
		return nil, err
	}
	b := make([]byte, frameHeaderSize, frameHeaderSize+len(stored))
	binary.BigEndian.PutUint32(b, uint32(len(stored)))
	binary.BigEndian.PutUint32(b[4:], uint32(len(plain)))
	b[8] = codec
	binary.BigEndian.PutUint64(b[9:], f.Generation)
	copy(b[17:], f.TxID[:])
	b = append(b, stored...)
	crc := crc32.Update(0, storeCRC, b[:33])
	crc = crc32.Update(crc, storeCRC, stored)
	binary.BigEndian.PutUint32(b[33:], crc)
	return b, nil
}

// decodeFrame consumes exactly one frame. io.EOF means no frame; unexpected EOF
// means a torn tail. All other errors are corruption, never a truncation hint.
func decodeFrame(r io.Reader) (f storeFrame, err error) {
	h := make([]byte, frameHeaderSize)
	if _, err = io.ReadFull(r, h); err != nil {
		return
	}
	n, p := binary.BigEndian.Uint32(h), binary.BigEndian.Uint32(h[4:])
	if n > frameLimit || p > frameLimit {
		return f, codecError()
	}
	b := make([]byte, int(n))
	if _, err = io.ReadFull(r, b); err != nil {
		return
	}
	crc := crc32.Update(0, storeCRC, h[:33])
	if crc32.Update(crc, storeCRC, b) != binary.BigEndian.Uint32(h[33:]) {
		return f, codecError()
	}
	if err = decodeStorePayload(b, uint64(p), h[8], frameLimit, &f); err != nil {
		return storeFrame{}, err
	}
	if f.Generation != binary.BigEndian.Uint64(h[9:]) || f.TxID != [16]byte(h[17:33]) {
		return storeFrame{}, codecError()
	}
	if err = f.validate(); err != nil {
		return storeFrame{}, err
	}
	return
}
