package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/transport"
)

func (d *Daemon) applyPeerDue(ctx context.Context, client *transport.Client, addr, peerID, short, epoch string, cursor uint64, due []folder.FileInfo, queue *peerQueue, progress folder.Progress) (int, int) {
	var small, singles []folder.FileInfo
	var bytes int64
	for _, fi := range due {
		if !fi.Deleted && fi.Size <= 256<<10 && d.folder.NeedsRemoteContent(short, fi) && len(small) < 64 && bytes+fi.Size <= 8<<20 {
			small = append(small, fi)
			bytes += fi.Size
		} else {
			singles = append(singles, fi)
		}
	}
	applied, failed := 0, 0
	if len(small) > 0 {
		items := make([]transport.BatchItem, 0, len(small))
		byID := make(map[string]folder.FileInfo, len(small))
		for i, fi := range small {
			id := fmt.Sprintf("%d", i)
			items = append(items, transport.BatchItem{ID: id, Name: fi.Name, Hash: fi.Hash, Size: fi.Size})
			byID[id] = fi
		}
		var changed []folder.FileInfo
		a, f, _ := d.folder.ApplyRemoteBatch(ctx, short, small, func(batchCtx context.Context) (map[string][]byte, error) {
			attemptCtx, cancel := context.WithTimeout(batchCtx, 20*time.Second)
			defer cancel()
			got, err := client.FetchFiles(attemptCtx, addr, items)
			bodies := make(map[string][]byte)
			for id, result := range got {
				if result.Header.Status == "ok" {
					bodies[byID[id].Name] = result.Body
				}
				if result.Header.Status == "version_changed" || result.Header.Status == "not_found" {
					changed = append(changed, byID[id])
				}
			}
			return bodies, err
		}, progress, func(fi folder.FileInfo, err error) {
			qe := queue.activeEpoch(peerID)
			if err != nil {
				queue.failed(peerID, qe, fi, time.Now())
			} else {
				queue.applied(peerID, qe, fi)
			}
		})
		applied += a
		failed += f
		for _, old := range changed {
			refreshPeerName(ctx, client, addr, peerID, epoch, cursor, old, queue)
		}
	}
	if len(singles) > 0 {
		var changed []folder.FileInfo
		a, f := d.folder.ApplyRemoteResults(ctx, short, singles, client.Fetcher(addr), progress, func(fi folder.FileInfo, err error) {
			qe := queue.activeEpoch(peerID)
			if err != nil {
				queue.failed(peerID, qe, fi, time.Now())
				// A known unavailable/stale response can race the index snapshot.
				// An exact refresh is harmless: only a returned record replaces work.
				if errors.Is(err, transport.ErrVersionChanged) || errors.Is(err, transport.ErrFileUnavailable) || errors.Is(err, transport.ErrPeerPaused) {
					changed = append(changed, fi)
				}
			} else {
				queue.applied(peerID, qe, fi)
			}
		})
		applied += a
		failed += f
		for _, old := range changed {
			refreshPeerName(ctx, client, addr, peerID, epoch, cursor, old, queue)
		}
	}
	return applied, failed
}

func refreshPeerName(ctx context.Context, client *transport.Client, addr, peerID, epoch string, cursor uint64, old folder.FileInfo, queue *peerQueue) {
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	idx, err := client.IndexSince(refreshCtx, addr, epoch, cursor, old.Name)
	cancel()
	if err == nil && idx != nil && len(idx.Files) > 0 {
		// Keep the accepted whole-index cursor intact if the named snapshot
		// observed later commits. The next ordinary poll will fetch that delta.
		queue.replaceName(peerID, idx.StoreEpoch, cursor, idx.Files[0])
	}
}
