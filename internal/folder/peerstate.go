package folder

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
)

var ErrPeerState = errors.New("invalid peer acknowledgement state")

func cloneControl(c storeControl) storeControl {
	c.Members = append([]storeMember(nil), c.Members...)
	c.GCFloor = append([]storePair(nil), c.GCFloor...)
	c.ACK = append([]storeAck(nil), c.ACK...)
	for i := range c.ACK {
		c.ACK[i].Version = append([]storePair(nil), c.ACK[i].Version...)
	}
	c.Retired = append([]storeRetired(nil), c.Retired...)
	for i := range c.Retired {
		c.Retired[i].Version = append([]storePair(nil), c.Retired[i].Version...)
	}
	if c.Blocked != nil {
		b := *c.Blocked
		c.Blocked = &b
	}
	return c
}

// appendControlLocked requires scanMu and saveMu. State is published by append
// only after the log flush; callers must never retain mutable aliases.
func (f *Folder) appendControlLocked(c storeControl, kind string) error {
	s := f.store
	if s == nil {
		return ErrPeerState
	}
	frame := storeFrame{Kind: kind, PreviousGeneration: s.state.Generation, Generation: s.state.Generation + 1, Clock: s.state.Clock, Control: &c}
	if _, err := rand.Read(frame.TxID[:]); err != nil {
		return err
	}
	if err := s.append(frame); err != nil {
		return err
	}
	f.mu.Lock()
	f.generation, f.durableGeneration = frame.Generation, frame.Generation
	f.mu.Unlock()
	return nil
}

func (f *Folder) SetPeers(ids []string) error {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] == id) {
			return ErrPeerState
		}
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return ErrPeerState
	}
	c := cloneControl(f.store.state.Control)
	same := len(ids) == len(c.Members)
	if same {
		for i := range ids {
			if ids[i] != c.Members[i].ID {
				same = false
				break
			}
		}
	}
	if same {
		return nil
	}
	if c.PeerSetEpoch == ^uint64(0) {
		return ErrPeerState
	}
	c.PeerSetEpoch++
	c.Members = make([]storeMember, len(ids))
	for i, id := range ids {
		c.Members[i].ID = id
	}
	c.ACK = nil
	return f.appendControlLocked(c, "Control")
}

func (f *Folder) PeerACKState() (string, uint64, []string) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return "", 0, nil
	}
	s := f.store.state
	ids := make([]string, len(s.Control.Members))
	for i, m := range s.Control.Members {
		ids[i] = m.ID
	}
	return hex.EncodeToString(s.StoreID[:]), s.Control.PeerSetEpoch, ids
}

// PeerACKEpoch returns the store and membership epochs without copying the peer list.
func (f *Folder) PeerACKEpoch() (string, uint64) {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return "", 0
	}
	return hex.EncodeToString(f.store.state.StoreID[:]), f.store.state.Control.PeerSetEpoch
}

// BeginPeerBaseline records a fresh challenge against a peer's store epoch.
// The caller issues it only with a full baseline and must bind it to that
// baseline's checkpoint; it cannot be inferred from an incoming index.
func (f *Folder) BeginPeerBaseline(peer, remoteEpoch, targetEpoch string, checkpoint uint64) (string, error) {
	if remoteEpoch == "" {
		return "", ErrPeerState
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return "", ErrPeerState
	}
	if targetEpoch != hex.EncodeToString(f.store.state.StoreID[:]) || checkpoint != f.store.state.Generation {
		return "", ErrPeerState
	}
	c := cloneControl(f.store.state.Control)
	i := sort.Search(len(c.Members), func(i int) bool { return c.Members[i].ID >= peer })
	if i == len(c.Members) || c.Members[i].ID != peer {
		return "", ErrPeerState
	}
	c.Members[i].RemoteEpoch, c.Members[i].Challenge, c.Members[i].Baseline, c.Members[i].Checkpoint = remoteEpoch, hex.EncodeToString(token[:]), false, checkpoint
	for j := len(c.ACK) - 1; j >= 0; j-- {
		if c.ACK[j].Peer == peer {
			c.ACK = append(c.ACK[:j], c.ACK[j+1:]...)
		}
	}
	if err := f.appendControlLocked(c, "Control"); err != nil {
		return "", err
	}
	return c.Members[i].Challenge, nil
}

// Acknowledge accepts only exact durable tombstones. Baseline evidence is the
// challenge issued for this peer and incarnation, not a caller-provided flag.
func (f *Folder) Acknowledge(peer, storeEpoch, remoteEpoch string, membershipEpoch uint64, deletions []FileInfo, baselineChallenge string) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return ErrPeerState
	}
	s := f.store.state
	if storeEpoch != hex.EncodeToString(s.StoreID[:]) || membershipEpoch != s.Control.PeerSetEpoch {
		return ErrPeerState
	}
	c := cloneControl(s.Control)
	i := sort.Search(len(c.Members), func(i int) bool { return c.Members[i].ID >= peer })
	if i == len(c.Members) || c.Members[i].ID != peer {
		return ErrPeerState
	}
	m := &c.Members[i]
	if remoteEpoch == "" || m.RemoteEpoch != remoteEpoch {
		return ErrPeerState
	}
	if baselineChallenge != "" {
		if m.Challenge == "" || baselineChallenge != m.Challenge {
			return ErrPeerState
		}
		m.Baseline = true
	}
	if !m.Baseline {
		return ErrPeerState
	}
	for _, fi := range deletions {
		if !fi.Deleted || !validName(fi.Name) || len(fi.Version) == 0 {
			return ErrPeerState
		}
		ix := sort.Search(len(s.Files), func(i int) bool { return s.Files[i].Name >= fi.Name })
		if ix == len(s.Files) || s.Files[ix].Name != fi.Name || !s.Files[ix].Deleted || !reflect.DeepEqual(s.Files[ix].Version, recoveryFile(&fi).Version) {
			return fmt.Errorf("%w: unknown deletion", ErrPeerState)
		}
		a := storeAck{Peer: peer, Name: fi.Name, Version: append([]storePair(nil), s.Files[ix].Version...)}
		j := sort.Search(len(c.ACK), func(j int) bool { return c.ACK[j].Peer > peer || (c.ACK[j].Peer == peer && c.ACK[j].Name >= fi.Name) })
		if j < len(c.ACK) && c.ACK[j].Peer == peer && c.ACK[j].Name == fi.Name {
			c.ACK[j] = a
		} else {
			c.ACK = append(c.ACK, storeAck{})
			copy(c.ACK[j+1:], c.ACK[j:])
			c.ACK[j] = a
		}
	}
	if reflect.DeepEqual(c, s.Control) {
		return nil
	}
	return f.appendControlLocked(c, "PeerAck")
}

// CanAcknowledgeDeletion checks durable local evidence without relying on
// receipt of an index. A concurrent edit does not cover the deletion.
func (f *Folder) CanAcknowledgeDeletion(deletion FileInfo) bool {
	if !deletion.Deleted || !validName(deletion.Name) || len(deletion.Version) == 0 {
		return false
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return false
	}
	retired := f.store.state.Control.Retired
	j := sort.Search(len(retired), func(i int) bool { return retired[i].Name >= deletion.Name })
	want := recoveryFile(&deletion).Version
	for ; j < len(retired) && retired[j].Name == deletion.Name; j++ {
		if reflect.DeepEqual(retired[j].Version, want) {
			return true
		}
	}
	files := f.store.state.Files
	i := sort.Search(len(files), func(i int) bool { return files[i].Name >= deletion.Name })
	if i == len(files) || files[i].Name != deletion.Name {
		return false
	}
	v := make(Vector, len(files[i].Version))
	for _, p := range files[i].Version {
		v[p.Peer] = p.Counter
	}
	order := v.Compare(deletion.Version)
	return (files[i].Deleted && order == Equal) || (!files[i].Deleted && order == Greater)
}

// NeedsPeerBaseline reports whether the current peer incarnation and remote
// store epoch have completed the checkpoint handshake.
func (f *Folder) NeedsPeerBaseline(peer, remoteEpoch string) bool {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return true
	}
	ms := f.store.state.Control.Members
	i := sort.Search(len(ms), func(i int) bool { return ms[i].ID >= peer })
	return i == len(ms) || ms[i].ID != peer || !ms[i].Baseline || ms[i].RemoteEpoch != remoteEpoch
}
