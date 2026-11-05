package folder

import (
	"context"
	"os"
)

// stagedApply owns only private downloads and pinned local descriptors. It has
// no authority to mutate user names or publish an index. Close/rebind callers
// must drain ApplyRemote, as they already do for downloads.
type stagedApply struct {
	tmp, disk, hash string
	local           *os.File
	info            os.FileInfo
}

func (s *stagedApply) close(f *Folder) {
	if s.local != nil {
		s.local.Close()
	}
	if s.tmp != "" {
		_ = f.root.unlink(s.tmp, false)
	}
}

// stageApply performs network and pre-intent hashing without scanMu/saveMu/mu.
// The forecast is never authoritative: applyOne repeats decide after staging.
func (f *Folder) stageApply(ctx context.Context, st step, fetch Fetcher) (_ *stagedApply, err error) {
	s := &stagedApply{disk: st.r.Name}
	defer func() {
		if err != nil {
			s.close(f)
		}
	}()
	if st.act != mergeOnly && st.needsDownload() {
		s.tmp, err = f.downloadOrReuse(ctx, st.r, fetch)
		if err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	live := false
	if l := f.files[st.r.Name]; l != nil {
		s.disk, live = l.diskPath(), !l.Deleted
	}
	f.mu.Unlock()
	if !validDisk(st.r.Name, s.disk) {
		return nil, errLocalChanged
	}
	if live {
		s.local, err = f.root.openRead(s.disk)
		if err != nil {
			return nil, err
		}
		s.hash, s.info, _, err = stableHash(s.local)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}
