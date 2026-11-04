package app

import (
	"math/rand"
	"sort"
	"sync"
	"time"

	"sharedir/internal/folder"
)

const (
	peerPendingLimit = 10000
	peerRetryMin     = 250 * time.Millisecond
	peerRetryMax     = 30 * time.Second
)

type peerQueue struct {
	mu      sync.Mutex
	pending map[peerItemKey]peerPending
	cursors map[string]peerCursor
	blocked map[string]int // slots that must drain before polling may resume
}

type peerItemKey struct{ peer, epoch, name string }
type peerCursor struct {
	epoch  string
	cursor uint64
}
type peerPending struct {
	info     folder.FileInfo
	attempts uint
	next     time.Time
}

func newPeerQueue() *peerQueue {
	return &peerQueue{pending: make(map[peerItemKey]peerPending), cursors: make(map[string]peerCursor), blocked: make(map[string]int)}
}

func (q *peerQueue) accept(peer, epoch string, cursor uint64, reset bool, files []folder.FileInfo) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = make(map[peerItemKey]peerPending)
	}
	if q.cursors == nil {
		q.cursors = make(map[string]peerCursor)
	}
	if q.blocked == nil {
		q.blocked = make(map[string]int)
	}
	old, known := q.cursors[peer]
	newEpoch := reset || (known && old.epoch != epoch)
	base := len(q.pending)
	if newEpoch {
		for key := range q.pending {
			if key.peer == peer {
				base--
			}
		}
	}
	adds := make(map[peerItemKey]peerPending, len(files))
	for _, fi := range files {
		fi.Disk = ""
		fi.Version = fi.Version.Copy()
		key := peerItemKey{peer, epoch, fi.Name}
		previous, exists := q.pending[key]
		if newEpoch {
			exists = false
		}
		if !exists {
			if _, duplicate := adds[key]; !duplicate {
				base++
			}
			adds[key] = peerPending{info: fi}
			continue
		}
		if samePeerVersion(previous.info, fi) {
			adds[key] = previous
		} else {
			adds[key] = peerPending{info: fi}
		}
	}
	if base > peerPendingLimit {
		q.blocked[peer] = base - len(q.pending)
		return false
	}
	if newEpoch {
		for key := range q.pending {
			if key.peer == peer {
				delete(q.pending, key)
			}
		}
	}
	for key, item := range adds {
		q.pending[key] = item
	}
	q.cursors[peer] = peerCursor{epoch: epoch, cursor: cursor}
	return true
}

func samePeerVersion(a, b folder.FileInfo) bool {
	return a.Hash == b.Hash && a.Version.Compare(b.Version) == folder.Equal
}

func (q *peerQueue) due(peer string, now time.Time, limit int) []folder.FileInfo {
	q.mu.Lock()
	defer q.mu.Unlock()
	keys := make([]peerItemKey, 0)
	for key, item := range q.pending {
		if key.peer == peer && !now.Before(item.next) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]folder.FileInfo, 0, len(keys))
	for _, key := range keys {
		fi := q.pending[key].info
		fi.Version = fi.Version.Copy()
		out = append(out, fi)
	}
	return out
}

func (q *peerQueue) failed(peer, epoch string, info folder.FileInfo, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := peerItemKey{peer, epoch, info.Name}
	item, ok := q.pending[key]
	if !ok || !samePeerVersion(item.info, info) {
		return
	}
	item.attempts++
	item.next = now.Add(peerRetryDelay(item.attempts))
	q.pending[key] = item
}

func peerRetryDelay(attempt uint) time.Duration {
	delay := peerRetryMin
	for i := uint(1); i < attempt && delay < peerRetryMax; i++ {
		delay *= 2
	}
	if delay > peerRetryMax {
		delay = peerRetryMax
	}
	// Positive jitter prevents peers from retrying in lockstep while preserving
	// the specified 250 ms…30 s envelope.
	span := int64(delay / 5)
	if span == 0 {
		return delay
	}
	jittered := delay + time.Duration(rand.Int63n(span+1))
	if jittered > peerRetryMax {
		return peerRetryMax
	}
	return jittered
}

func (q *peerQueue) applied(peer, epoch string, info folder.FileInfo) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := peerItemKey{peer, epoch, info.Name}
	if item, ok := q.pending[key]; ok && samePeerVersion(item.info, info) {
		delete(q.pending, key)
		for blockedPeer, slots := range q.blocked {
			if slots > 1 {
				q.blocked[blockedPeer] = slots - 1
			} else {
				delete(q.blocked, blockedPeer)
			}
		}
	}
}

func (q *peerQueue) cursor(peer string) (epoch string, cursor uint64, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c, ok := q.cursors[peer]
	return c.epoch, c.cursor, ok
}

func (q *peerQueue) canPoll(peer string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.blocked[peer] == 0 && len(q.pending) < peerPendingLimit
}
