package app

import (
	"context"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/transport"
)

type folderPeerState struct{ folder *folder.Folder }

func (s folderPeerState) Hello() transport.Hello {
	epoch, _ := s.folder.DurableCursor()
	// Membership is not durable/modelled until wave C; keep a stable placeholder.
	return transport.Hello{Protocol: transport.ProtocolVersion, StoreEpoch: epoch, PeerSetEpoch: "membership-v1-placeholder", Features: []string{"files", "gzip"}}
}

func (s folderPeerState) Index(ctx context.Context, epoch string, since uint64, wait time.Duration, name string) (transport.IndexResponse, error) {
	files, currentEpoch, cursor := s.folder.DurableState()
	out := transport.IndexResponse{Protocol: transport.ProtocolVersion, StoreEpoch: currentEpoch, Cursor: cursor}
	if name != "" {
		for _, fi := range files {
			if fi.Name == name {
				fi.Disk = ""
				fi.StoreDeletedAt = 0
				out.Files = append(out.Files, fi)
			}
		}
		return out, nil
	}
	if epoch != currentEpoch || since > cursor || since < cursor {
		out.Reset = true
	} else if wait > 0 {
		if err := s.folder.WaitDurable(ctx, epoch, since, wait); err != nil {
			return transport.IndexResponse{}, err
		}
		files, currentEpoch, cursor = s.folder.DurableState()
		out.StoreEpoch, out.Cursor = currentEpoch, cursor
		out.Reset = epoch != currentEpoch || since > cursor || since < cursor
	}
	if out.Reset || since != cursor {
		for _, fi := range files {
			fi.Disk = ""
			fi.StoreDeletedAt = 0
			out.Files = append(out.Files, fi)
		}
	}
	return out, nil
}
