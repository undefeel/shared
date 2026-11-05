package folder

import (
	"crypto/rand"
	"reflect"
	"sort"
	"time"
)

const tombstoneRetention = 30 * 24 * time.Hour

func retiredLess(a, b storeRetired) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	for i := 0; i < len(a.Version) && i < len(b.Version); i++ {
		if a.Version[i].Peer != b.Version[i].Peer {
			return a.Version[i].Peer < b.Version[i].Peer
		}
		if a.Version[i].Counter != b.Version[i].Counter {
			return a.Version[i].Counter < b.Version[i].Counter
		}
	}
	return len(a.Version) < len(b.Version)
}

// retiredCoversLocked is called under saveMu and scanMu before touching disk.
func (f *Folder) retiredCoversLocked(fi FileInfo) bool {
	if f.store == nil {
		return false
	}
	rs := f.store.state.Control.Retired
	i := sort.Search(len(rs), func(i int) bool { return rs[i].Name >= fi.Name })
	for ; i < len(rs) && rs[i].Name == fi.Name; i++ {
		v := make(Vector, len(rs[i].Version))
		for _, p := range rs[i].Version {
			v[p.Peer] = p.Counter
		}
		order := v.Compare(fi.Version)
		if order == Equal || order == Greater {
			return true
		}
	}
	return false
}

func (f *Folder) retiredCovers(fi FileInfo) bool {
	if !f.retiredAvailable.Load() {
		return false
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.retiredCoversLocked(fi)
}

// CollectTombstones retains exact retired versions permanently and publishes
// removal only after one durable frame has committed both effects.
func (f *Folder) CollectTombstones(now time.Time) (int, error) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	if f.store == nil {
		return 0, ErrPeerState
	}
	if !time.Unix(0, now.UnixNano()).Equal(now) {
		return 0, nil
	}
	if e := f.Blocked(); e != nil {
		return 0, e
	}
	s := f.store.state
	c := cloneControl(s.Control)
	if len(c.Members) == 0 {
		return 0, nil
	}
	frame := storeFrame{Kind: "GC", PreviousGeneration: s.Generation, Generation: s.Generation + 1, Clock: s.Clock, Control: &c}
	for _, v := range s.Files {
		if !v.Deleted || v.DeletedAt <= 0 || len(v.Version) == 0 {
			continue
		}
		observed := time.Unix(0, v.DeletedAt)
		if now.Before(observed) || now.Sub(observed) < tombstoneRetention {
			continue
		}
		eligible := true
		for _, m := range c.Members {
			if !m.Baseline {
				eligible = false
				break
			}
			i := sort.Search(len(c.ACK), func(i int) bool { return c.ACK[i].Peer > m.ID || (c.ACK[i].Peer == m.ID && c.ACK[i].Name >= v.Name) })
			if i == len(c.ACK) || c.ACK[i].Peer != m.ID || c.ACK[i].Name != v.Name || !reflect.DeepEqual(c.ACK[i].Version, v.Version) {
				eligible = false
				break
			}
		}
		if !eligible {
			continue
		}
		r := storeRetired{Name: v.Name, Version: append([]storePair(nil), v.Version...)}
		i := sort.Search(len(c.Retired), func(i int) bool { return !retiredLess(c.Retired[i], r) })
		if i == len(c.Retired) || retiredLess(r, c.Retired[i]) {
			c.Retired = append(c.Retired, storeRetired{})
			copy(c.Retired[i+1:], c.Retired[i:])
			c.Retired[i] = r
		}
		frame.Operations = append(frame.Operations, storeOperation{Name: v.Name, Before: storeDigest(v), DeleteRecord: true})
	}
	if len(frame.Operations) == 0 {
		return 0, nil
	}
	if _, err := rand.Read(frame.TxID[:]); err != nil {
		return 0, err
	}
	if err := f.store.append(frame); err != nil {
		return 0, err
	}
	f.retiredAvailable.Store(true)
	f.mu.Lock()
	for _, op := range frame.Operations {
		delete(f.files, op.Name)
	}
	f.generation, f.durableGeneration = frame.Generation, frame.Generation
	f.notify()
	f.mu.Unlock()
	return len(frame.Operations), nil
}
