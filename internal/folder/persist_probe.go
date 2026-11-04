package folder

import "sync"

// PersistenceMeasurement observes real I/O without replacing it. Install Probe
// before concurrent Folder use. Snapshots are safe during concurrent callbacks.
type PersistenceMeasurement struct {
	mu     sync.Mutex
	bytes  map[string]uint64
	groups uint64
}

func (m *PersistenceMeasurement) Probe() PersistenceProbe {
	return PersistenceProbe{Write: func(kind, _ string, n int, _ error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.bytes == nil {
			m.bytes = make(map[string]uint64)
		}
		if n > 0 {
			m.bytes[kind] += uint64(n)
		}
	}, Phase: func(_, phase string) {
		if phase == "G5" {
			m.mu.Lock()
			m.groups++
			m.mu.Unlock()
		}
	}}
}

// Snapshot returns actual bytes by persistence kind and durable group count.
func (m *PersistenceMeasurement) Snapshot() (map[string]uint64, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := make(map[string]uint64, len(m.bytes))
	for k, v := range m.bytes {
		b[k] = v
	}
	return b, m.groups
}
