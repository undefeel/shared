package folder

import (
	"sync"
	"time"
)

const maxStabilityProbes = 10000

// stabilityProbe retains the last observed metadata and its eligibility time.
// Ready probes remain cached as evidence until metadata changes.
type stabilityProbe struct {
	stat     scanStat
	deadline time.Time
	ready    bool
}

// stabilityTracker tracks whether a file's metadata has remained unchanged
// for the configured interval. It performs no I/O and never waits under lock.
type stabilityTracker struct {
	mu               sync.Mutex
	now              func() time.Time
	interval         time.Duration
	probes           map[string]stabilityProbe
	ready            map[string]scanStat
	fullRescanNeeded bool
}

func newStabilityTracker() *stabilityTracker {
	return &stabilityTracker{
		now:      time.Now,
		interval: time.Second,
		probes:   make(map[string]stabilityProbe),
		ready:    make(map[string]scanStat),
	}
}

func (s *stabilityTracker) initLocked() {
	if s.now == nil {
		s.now = time.Now
	}
	if s.interval <= 0 {
		s.interval = time.Second
	}
	if s.probes == nil {
		s.probes = make(map[string]stabilityProbe)
	}
	if s.ready == nil {
		s.ready = make(map[string]scanStat)
	}
}

// probe returns true once name has had identical metadata for interval.
// Metadata equality intentionally includes ctime and mode as well as the
// required size, mtime, device, inode, and executable bit.
func (s *stabilityTracker) probe(name string, stat scanStat) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	now := s.now()
	if oldStat, ok := s.ready[name]; ok {
		if oldStat == stat {
			return true
		}
		delete(s.ready, name)
	}
	if old, ok := s.probes[name]; ok {
		if old.stat == stat {
			if old.ready || !now.Before(old.deadline) {
				delete(s.probes, name)
				s.ready[name] = stat
				return true
			}
			return false
		}
		s.probes[name] = stabilityProbe{stat: stat, deadline: now.Add(s.interval)}
		return false
	}
	if len(s.probes) >= maxStabilityProbes {
		s.fullRescanNeeded = true
		return false
	}
	s.probes[name] = stabilityProbe{stat: stat, deadline: now.Add(s.interval)}
	return false
}

func (s *stabilityTracker) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	n = len(s.probes)
	return n
}

func (s *stabilityTracker) needsFullRescan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fullRescanNeeded
}

// reconcile is called only after a successful complete scan. It drops probes
// for paths that disappeared and clears overflow so a later scan can refill.
func (s *stabilityTracker) reconcile(present map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	for name := range s.probes {
		if _, ok := present[name]; !ok {
			delete(s.probes, name)
		}
	}
	for name := range s.ready {
		if _, ok := present[name]; !ok {
			delete(s.ready, name)
		}
	}
	s.fullRescanNeeded = false
}

func (f *Folder) stabilityState() *stabilityTracker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stability == nil {
		f.stability = newStabilityTracker()
	}
	return f.stability
}

// PendingStability reports the number of observed files still inside their
// stability window.
func (f *Folder) PendingStability() int {
	f.mu.Lock()
	s := f.stability
	f.mu.Unlock()
	if s == nil {
		return 0
	}
	return s.pending()
}

// StabilityFullRescanNeeded reports whether probe capacity overflowed and a
// successful full scan must reconcile the retained evidence.
func (f *Folder) StabilityFullRescanNeeded() bool {
	f.mu.Lock()
	s := f.stability
	f.mu.Unlock()
	return s != nil && s.needsFullRescan()
}
