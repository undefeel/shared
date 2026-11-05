package app

import (
	"time"

	"sharedir/internal/folder"
)

const (
	watchHealthyInterval  = 12 * time.Minute
	watchFallbackInterval = time.Minute
	maxWatchPaths         = 10000
)

// scanQueue coalesces watcher callbacks without ever blocking the watcher.
// Once the path bound is exceeded, a full scan subsumes the entire queue.
type scanQueue struct {
	paths      map[string]struct{}
	full       bool
	root       bool
	generation uint64
}

func (q *scanQueue) merge(b folder.WatchBatch) {
	if b.FullRequired || b.RootChanged {
		q.full = q.full || b.FullRequired
		q.root = q.root || b.RootChanged
		q.generation = b.Generation
		q.paths = nil
		return
	}
	if q.full || q.root {
		return
	}
	if q.paths == nil {
		q.paths = make(map[string]struct{})
	}
	for _, p := range b.Paths {
		q.paths[p] = struct{}{}
		if len(q.paths) > maxWatchPaths {
			q.paths = nil
			q.full = true
			break
		}
	}
	q.generation = b.Generation
}

func (q *scanQueue) drain() folder.WatchBatch {
	b := folder.WatchBatch{FullRequired: q.full, RootChanged: q.root, Generation: q.generation}
	for p := range q.paths {
		b.Paths = append(b.Paths, p)
	}
	q.paths = nil
	q.full = false
	q.root = false
	return b
}

// scanDeadline is traffic independent: events cannot move the periodic deadline.
type scanDeadline struct {
	at      time.Time
	healthy bool
}

func (d *scanDeadline) set(now time.Time, healthy bool) {
	if d.at.IsZero() || d.healthy != healthy {
		d.healthy = healthy
		d.at = now.Add(watchHealthyInterval)
		if !healthy {
			d.at = now.Add(watchFallbackInterval)
		}
	}
}
func (d *scanDeadline) due(now time.Time) bool { return !now.Before(d.at) }
func (d *scanDeadline) advance(now time.Time) {
	if d.healthy {
		d.at = d.at.Add(watchHealthyInterval)
	} else {
		d.at = d.at.Add(watchFallbackInterval)
	}
	if !d.at.After(now) {
		interval := watchFallbackInterval
		if d.healthy {
			interval = watchHealthyInterval
		}
		d.at = now.Add(interval)
	}
}
