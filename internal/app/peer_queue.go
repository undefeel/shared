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
	blocked map[string]bool
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

// peerIndex retains the work represented by a cursor until every record has
// been applied. Replaying the same index can fill available slots without
// re-enqueuing records that have already completed.
type peerIndex struct {
	epoch   string
	cursor  uint64
	records map[string]peerIndexRecord
}

type peerIndexRecord struct {
	info folder.FileInfo
	done bool
}

func newPeerQueue() *peerQueue {
	return &peerQueue{pending: make(map[peerItemKey]peerPending), cursors: make(map[string]peerCursor), indexes: make(map[string]peerIndex), blocked: make(map[string]bool)}
}

// accept queues candidates; false may queue a prefix, but never advances cursor.
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
	if q.blocked == nil {
		q.blocked = make(map[string]bool)
	}
	index, haveIndex := q.indexes[peer]
	newEpoch := haveIndex && index.epoch != epoch
	if newEpoch {
		for key := range q.pending {
			if key.peer == peer {
				delete(q.pending, key)
			}
		}
		delete(q.blocked, peer)
		delete(q.indexes, peer)
		delete(q.cursors, peer)
		if len(q.pending) < peerPendingLimit {
			clear(q.blocked)
		}
		haveIndex = false
	}
	// A reset bit can accompany a retry. Only a changed epoch or cursor starts
	// a new index continuation.
	newIndex := !haveIndex || index.epoch != epoch || index.cursor != cursor
	if newIndex {
		index = peerIndex{epoch: epoch, cursor: cursor, records: make(map[string]peerIndexRecord)}
	}
	left := false
	for _, fi := range files {
		fi.Disk = ""
		fi.Version = fi.Version.Copy()
		record, exists := index.records[fi.Name]
		if exists {
			cmp := record.info.Version.Compare(fi.Version)
			if cmp == folder.Greater {
				fi = record.info
			} else if !samePeerVersion(record.info, fi) {
				record.info = fi
				record.done = false
			}
		}
		if !exists {
			record.info = fi
		}
		index.records[fi.Name] = record
		key := peerItemKey{peer, epoch, fi.Name}
		pending, queued := q.pending[key]
		if queued && samePeerVersion(pending.info, fi) {
			continue
		}
		if record.done && samePeerVersion(record.info, fi) {
			continue
		}
		if !queued && len(q.pending) >= peerPendingLimit {
			left = true
			continue
		}
		q.pending[key] = peerPending{info: fi}
	}
	q.indexes[peer] = index
	if left {
		q.blocked[peer] = true
		return false
	}
	delete(q.blocked, peer)
	for _, record := range index.records {
		if !record.done {
			return true
		}
	}
	q.cursors[peer] = peerCursor{epoch: epoch, cursor: cursor}
	delete(q.blocked, peer)
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
		if index, exists := q.indexes[peer]; exists && index.epoch == epoch {
			if record, exists := index.records[info.Name]; exists && samePeerVersion(record.info, info) {
				record.done = true
				index.records[info.Name] = record
				q.indexes[peer] = index
				complete := true
				for _, current := range index.records {
					if !current.done {
						complete = false
						break
					}
				}
				if complete {
					q.cursors[peer] = peerCursor{epoch: index.epoch, cursor: index.cursor}
				}
			}
		}
		if len(q.pending) < peerPendingLimit {
			clear(q.blocked)
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
	if q.blocked[peer] || len(q.pending) >= peerPendingLimit {
		return false
	}
	index, exists := q.indexes[peer]
	if !exists {
		return true
	}
	for name, record := range index.records {
		if record.done {
			continue
		}
		if _, queued := q.pending[peerItemKey{peer: peer, epoch: index.epoch, name: name}]; !queued {
			// The same index still has a suffix that has not entered the queue.
			return true
		}
	}
	// All records are queued or complete. Waiting for application/backoff here
	// avoids repeatedly fetching the same index while work is already pending.
	for _, record := range index.records {
		if !record.done {
			return false
		}
	}
	return true
}
