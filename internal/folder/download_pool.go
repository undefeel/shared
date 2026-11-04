package folder

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

const (
	defaultDownloadConcurrency       = 6
	maxStagedOperations              = 128
	maxStagedBytes             int64 = 32 << 20
)

// downloadPool accounts for network streams and all incoming work from the
// moment it is planned until its durable coordinator result has settled.
type downloadPool struct {
	mu                   sync.Mutex
	streams, streamLimit int
	operations           int
	bytes                int64
	oversized            bool
	streamWaiters        []*poolWaiter
	stageWaiters         []*poolWaiter
}

// ApplyRemoteBatch reserves the batch's staging footprint before invoking
// fetchBatch. fetchBatch must return verified plain bodies keyed by canonical
// name; reservations remain held through durable apply and cleanup.
func (f *Folder) ApplyRemoteBatch(ctx context.Context, peer string, records []FileInfo, fetchBatch func(context.Context) (map[string][]byte, error), progress Progress, outcome func(FileInfo, error)) (int, int, error) {
	var total int64
	for _, fi := range records {
		if !fi.Deleted {
			if fi.Size < 0 || total > int64(^uint64(0)>>1)-fi.Size {
				return 0, len(records), errors.New("invalid batch size")
			}
			total += fi.Size
		}
	}
	releaseStage, err := f.reserveStagingCount(ctx, total, len(records))
	if err != nil {
		return 0, len(records), err
	}
	defer releaseStage()
	releaseStream, err := f.acquireDownload(ctx)
	if err != nil {
		return 0, len(records), err
	}
	bodies, fetchErr := fetchBatch(ctx)
	releaseStream()
	fetch := func(_ context.Context, fi FileInfo, w io.Writer) error {
		body, ok := bodies[fi.Name]
		if !ok {
			return errors.New("batch body unavailable")
		}
		_, err := io.Copy(w, bytes.NewReader(body))
		return err
	}
	applied, failed := f.applyRemoteResults(ctx, peer, records, fetch, progress, outcome, true)
	return applied, failed, fetchErr
}

type poolWaiter struct {
	ready  chan struct{}
	stream bool
	bytes  int64
	ops    int
}

func (f *Folder) pool() *downloadPool {
	f.downloadMu.Lock()
	defer f.downloadMu.Unlock()
	if f.downloadPool == nil {
		f.downloadPool = &downloadPool{streamLimit: defaultDownloadConcurrency}
	}
	return f.downloadPool
}

// SetDownloadConcurrency configures the shared per-Folder stream limit before
// peers start. Invalid values preserve the safe default.
func (f *Folder) SetDownloadConcurrency(n int) {
	if n < 1 || n > 8 {
		n = defaultDownloadConcurrency
	}
	f.downloadMu.Lock()
	if f.downloadPool == nil {
		f.downloadPool = &downloadPool{streamLimit: n}
	} else {
		f.downloadPool.mu.Lock()
		if f.downloadPool.streams == 0 && len(f.downloadPool.streamWaiters) == 0 {
			f.downloadPool.streamLimit = n
		}
		f.downloadPool.mu.Unlock()
	}
	f.downloadMu.Unlock()
}

func (p *downloadPool) signal() {
	for len(p.streamWaiters) > 0 && p.streams < p.streamLimit {
		w := p.streamWaiters[0]
		p.streams++
		p.streamWaiters = p.streamWaiters[1:]
		close(w.ready)
	}
	for len(p.stageWaiters) > 0 {
		w := p.stageWaiters[0]
		if p.operations+w.ops > maxStagedOperations || p.oversized || (w.bytes > maxStagedBytes && (w.ops != 1 || p.operations != 0 || p.bytes != 0)) || (w.bytes <= maxStagedBytes && p.bytes+w.bytes > maxStagedBytes) {
			return
		}
		p.operations += w.ops
		if w.bytes > maxStagedBytes {
			p.oversized = true
		} else {
			p.bytes += w.bytes
		}
		p.stageWaiters = p.stageWaiters[1:]
		close(w.ready)
	}
}

func (p *downloadPool) acquire(ctx context.Context, stream bool, bytes int64, ops int) (func(), error) {
	if bytes < 0 {
		return nil, errors.New("negative staging reservation")
	}
	p.mu.Lock()
	w := &poolWaiter{ready: make(chan struct{}), stream: stream, bytes: bytes, ops: ops}
	if stream {
		p.streamWaiters = append(p.streamWaiters, w)
	} else {
		p.stageWaiters = append(p.stageWaiters, w)
	}
	p.signal()
	p.mu.Unlock()
	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			p.mu.Lock()
			p.releaseLocked(stream, bytes, ops)
			p.signal()
			p.mu.Unlock()
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				p.mu.Lock()
				p.releaseLocked(stream, bytes, ops)
				p.signal()
				p.mu.Unlock()
			})
		}, nil
	case <-ctx.Done():
		p.mu.Lock()
		waiting := false
		waiters := &p.stageWaiters
		if stream {
			waiters = &p.streamWaiters
		}
		for i, pending := range *waiters {
			if pending == w {
				*waiters = append((*waiters)[:i], (*waiters)[i+1:]...)
				waiting = true
				break
			}
		}
		if !waiting {
			p.releaseLocked(stream, bytes, ops)
		}
		p.signal()
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *downloadPool) releaseLocked(stream bool, bytes int64, ops int) {
	if stream {
		p.streams--
		return
	}
	p.operations -= ops
	if bytes > maxStagedBytes {
		p.oversized = false
	} else {
		p.bytes -= bytes
	}
}

func (f *Folder) reserveStaging(ctx context.Context, bytes int64) (func(), error) {
	return f.reserveStagingCount(ctx, bytes, 1)
}
func (f *Folder) reserveStagingCount(ctx context.Context, bytes int64, ops int) (func(), error) {
	return f.pool().acquire(ctx, false, bytes, ops)
}
func (f *Folder) acquireDownload(ctx context.Context) (func(), error) {
	return f.pool().acquire(ctx, true, 0, 0)
}

// DurableState returns a detached, canonical index snapshot and its durable
// store epoch/generation. Lock order is scanMu -> saveMu -> mu.
func (f *Folder) DurableState() ([]FileInfo, string, uint64) {
	f.scanMu.Lock()
	f.saveMu.Lock()
	f.mu.Lock()
	files := make([]FileInfo, 0, len(f.files))
	for _, fi := range f.files {
		c := *fi
		c.Version = fi.Version.Copy()
		files = append(files, c)
	}
	epoch, cursor := hex.EncodeToString(f.store.state.StoreID[:]), f.durableGeneration
	f.mu.Unlock()
	f.saveMu.Unlock()
	f.scanMu.Unlock()
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, epoch, cursor
}

// WaitDurable blocks until the durable cursor or epoch differs, timeout elapses,
// or ctx is canceled. It captures the wake channel before reading the durable
// snapshot so a commit cannot be lost between snapshot and wait registration.
func (f *Folder) WaitDurable(ctx context.Context, epoch string, cursor uint64, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		changed := f.changed
		f.mu.Unlock()
		_, currentEpoch, currentCursor := f.DurableState()
		if currentEpoch != epoch || currentCursor != cursor {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		}
	}
}
