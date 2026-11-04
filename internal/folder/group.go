package folder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	groupCount       = 64
	groupBytes int64 = 8 << 20
	queueCount       = 128
	queueBytes int64 = 32 << 20
	groupDelay       = 25 * time.Millisecond
)

// groupRequest owns staged data until submit returns. Names includes every
// canonical target reserved by the operation (including rename/conflict names).
// The commit callback rechecks Context, binding, generation and decisions under
// scanMu -> saveMu before intent; after intent it must finish without cancellation.
// Generation identifies the confirmed binding epoch, not the store generation.
type groupRequest struct {
	Context    context.Context
	Names      []string
	Generation uint64
	Bytes      int64
	ready      time.Time
	done       chan error
}

// groupCoordinator serializes ready operations from all peers. The callback is
// the sole consumer; it reports success only after durable group publication.
// No Folder lock is held while waiting, collecting, or calling commit.
type groupCoordinator struct {
	mu      sync.Mutex
	changed chan struct{}
	stopped chan struct{}
	closed  bool
	pending []*groupRequest
	count   int
	bytes   int64
	commit  func([]*groupRequest) error
}

func newGroupCoordinator(commit func([]*groupRequest) error) *groupCoordinator {
	c := &groupCoordinator{changed: make(chan struct{}), stopped: make(chan struct{}), commit: commit}
	go c.run()
	return c
}

var errGroupClosed = errors.New("group coordinator closed")

func (c *groupCoordinator) signal() { close(c.changed); c.changed = make(chan struct{}) }

func (c *groupCoordinator) submit(ctx context.Context, names []string, generation uint64, bytes int64) error {
	if bytes < 0 || len(names) == 0 {
		return errors.New("invalid group request")
	}
	r := &groupRequest{Context: ctx, Names: append([]string(nil), names...), Generation: generation, Bytes: bytes, done: make(chan error, 1)}
	for {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return err
		}
		if c.closed {
			c.mu.Unlock()
			return errGroupClosed
		}
		// Include the active batch in staging accounting. A large file is allowed
		// only when it is the sole outstanding operation.
		if c.count < queueCount && ((bytes <= queueBytes && c.bytes <= queueBytes-bytes) || c.count == 0) {
			r.ready = time.Now()
			c.pending = append(c.pending, r)
			c.count++
			c.bytes += bytes
			c.signal()
			c.mu.Unlock()
			// Cancellation wakes collection, but never lets a caller dispose staging
			// while an intent-bearing commit may still be using it.
			stop := context.AfterFunc(ctx, func() { c.mu.Lock(); c.signal(); c.mu.Unlock() })
			defer stop()
			return <-r.done
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// close stops admission and drains admitted work. cancelPending cancels only
// work not yet dispatched; an active commit is always drained, including G2+.
// Call without daemon.mu/scanMu/saveMu held.
func (c *groupCoordinator) close(cancelPending bool) {
	c.mu.Lock()
	c.closed = true
	if cancelPending {
		for _, r := range c.pending {
			c.finish(r, context.Canceled)
		}
		c.pending = nil
	}
	c.signal()
	c.mu.Unlock()
	<-c.stopped
}

// finish requires mu.
func (c *groupCoordinator) finish(r *groupRequest, err error) {
	c.count--
	c.bytes -= r.Bytes
	r.done <- err
}

func groupOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (c *groupCoordinator) run() {
	defer close(c.stopped)
	for {
		c.mu.Lock()
		// Canceled staging has no durable intent and can be released immediately.
		live := c.pending[:0]
		for _, r := range c.pending {
			if err := r.Context.Err(); err != nil {
				c.finish(r, err)
			} else {
				live = append(live, r)
			}
		}
		c.pending = live
		c.signal()
		if len(live) == 0 {
			if c.closed {
				c.mu.Unlock()
				return
			}
			ch := c.changed
			c.mu.Unlock()
			<-ch
			continue
		}
		n := 0
		var size int64
		var names []string
		for _, r := range live {
			split := n == groupCount || (n > 0 && (r.Generation != live[0].Generation || r.Bytes > groupBytes-size))
			for _, a := range names {
				for _, b := range r.Names {
					if groupOverlap(a, b) {
						split = true
					}
				}
			}
			if split {
				break
			}
			n++
			size += r.Bytes
			names = append(names, r.Names...)
			if size >= groupBytes {
				break
			}
		}
		delay := time.Until(live[0].ready.Add(groupDelay))
		if !c.closed && n == len(live) && n < groupCount && size < groupBytes && delay > 0 {
			ch := c.changed
			c.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-ch:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		batch := append([]*groupRequest(nil), live[:n]...)
		c.pending = append([]*groupRequest(nil), live[n:]...)
		c.mu.Unlock()
		err := c.commit(batch)
		c.mu.Lock()
		for _, r := range batch {
			c.finish(r, err)
		}
		c.signal()
		c.mu.Unlock()
	}
}
