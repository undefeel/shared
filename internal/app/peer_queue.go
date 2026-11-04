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
	indexes map[string]peerIndex
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

// peerIndex keeps only constant-size continuation state. FileInfo values live
// in pending only until durable apply; replay advances from boundary.
type peerIndex struct {
	epoch     string
	cursor    uint64
	boundary  string
	windowEnd string
	remaining int
	exhausted bool
}

func newPeerQueue() *peerQueue {
	return &peerQueue{pending: make(map[peerItemKey]peerPending), cursors: make(map[string]peerCursor), indexes: make(map[string]peerIndex)}
}

// accept queues one bounded sorted window. The caller replays the same stable
// index only after that window drains, so continuation uses a canonical name.
func (q *peerQueue) accept(peer, epoch string, cursor uint64, reset bool, files []folder.FileInfo) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = make(map[peerItemKey]peerPending)
	}
	if q.cursors == nil {
		q.cursors = make(map[string]peerCursor)
	}
	if q.indexes == nil {
		q.indexes = make(map[string]peerIndex)
	}
	index, ok := q.indexes[peer]
	if !ok || index.epoch != epoch || index.cursor != cursor {
		for key := range q.pending {
			if key.peer == peer {
				delete(q.pending, key)
			}
		}
		delete(q.cursors, peer)
		index = peerIndex{epoch: epoch, cursor: cursor}
	}
	if index.remaining != 0 {
		q.indexes[peer] = index
		return false
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	added := 0
	var end string
	more := false
	space := peerPendingLimit - len(q.pending)
	for _, fi := range files {
		if fi.Name <= index.boundary {
			continue
		}
		if added == space {
			more = true
			break
		}
		fi.Disk = ""
		fi.Version = fi.Version.Copy()
		fiKey := peerItemKey{peer, epoch, fi.Name}
		q.pending[fiKey] = peerPending{info: fi}
		end = fi.Name
		added++
	}
	if added > 0 {
		index.windowEnd, index.remaining, index.exhausted = end, added, !more
		q.indexes[peer] = index
		return !more
	}
	if more {
		index.exhausted = false
		q.indexes[peer] = index
		return false
	}
	index.exhausted = true
	q.indexes[peer] = index
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
	span := int64(delay / 5)
	if span == 0 {
		return delay
	}
	if delay == peerRetryMax {
		return delay - time.Duration(rand.Int63n(span+1))
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
		index, exists := q.indexes[peer]
		if exists && index.epoch == epoch && index.remaining > 0 {
			index.remaining--
			if index.remaining == 0 {
				index.boundary = index.windowEnd
				if index.exhausted {
					q.cursors[peer] = peerCursor{epoch: index.epoch, cursor: index.cursor}
				}
			}
			q.indexes[peer] = index
		}
	}
}
func (q *peerQueue) cursor(peer string) (string, uint64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c, ok := q.cursors[peer]
	return c.epoch, c.cursor, ok
}
func (q *peerQueue) activeEpoch(peer string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.indexes[peer].epoch
}
func (q *peerQueue) activeCursor(peer string) uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.indexes[peer].cursor
}

// replaceName refreshes only an outstanding item. A changed epoch/cursor is a
// baseline boundary and causes the next ordinary poll to fetch a complete index.
func (q *peerQueue) replaceName(peer, epoch string, cursor uint64, fi folder.FileInfo) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	index, ok := q.indexes[peer]
	if !ok {
		return false
	}
	if index.epoch != epoch || index.cursor != cursor {
		for key := range q.pending {
			if key.peer == peer {
				delete(q.pending, key)
			}
		}
		delete(q.indexes, peer)
		delete(q.cursors, peer)
		return false
	}
	fi.Disk = ""
	fi.Version = fi.Version.Copy()
	key := peerItemKey{peer, epoch, fi.Name}
	if old, exists := q.pending[key]; exists {
		q.pending[key] = peerPending{info: fi, attempts: old.attempts}
		return true
	}
	return false
}
func (q *peerQueue) canPoll(peer string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	index, exists := q.indexes[peer]
	if !exists {
		return true
	}
	return index.remaining == 0 && len(q.pending) < peerPendingLimit
}
