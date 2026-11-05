package folder

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// storeDigest hashes the JSON encoding of the fixed storeFile struct. Version
// pairs are validated in peer order; nil and empty vectors have one encoding.
// Unlike gob type IDs, these bytes do not depend on process encoding history.
func storeDigest(f storeFile) [32]byte {
	if len(f.Version) == 0 {
		f.Version = nil
	}
	b, _ := json.Marshal(f)
	return sha256.Sum256(b)
}
func applyStoreFrame(s storeSnapshot, f storeFrame) (storeSnapshot, error) {
	if err := f.validate(); err != nil {
		return s, err
	}
	if f.PreviousGeneration != s.Generation || f.Clock < s.Clock {
		return s, codecError()
	}
	if len(f.Operations) == 0 {
		s.Generation, s.Clock = f.Generation, f.Clock
		if f.Control != nil {
			s.Control = *f.Control
		}
		return s, nil
	}
	files := make(map[string]storeFile, len(s.Files))
	for _, v := range s.Files {
		files[v.Name] = v
	}
	for _, op := range f.Operations {
		var digest [32]byte
		if old, ok := files[op.Name]; ok {
			digest = storeDigest(old)
		}
		if digest != op.Before {
			return s, fmt.Errorf("store Before mismatch: %s", op.Name)
		}
		if op.DeleteRecord {
			delete(files, op.Name)
		} else {
			files[op.Name] = *op.After
		}
	}
	s.Files = make([]storeFile, 0, len(files))
	for _, v := range files {
		s.Files = append(s.Files, v)
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Name < s.Files[j].Name })
	s.Generation, s.Clock = f.Generation, f.Clock
	if f.Control != nil {
		s.Control = *f.Control
	}
	return s, nil
}

// Replay verifies even superseded frames left by snapshot compaction. Only a
// physically incomplete final frame is discardable; a complete bad CRC blocks.
func replayStore(r io.Reader, snapshot storeSnapshot) (storeSnapshot, int64, bool, error) {
	if err := snapshot.validate(); err != nil {
		return snapshot, 0, false, err
	}
	h, err := decodeLogHeader(r)
	if err != nil {
		return snapshot, 0, false, err
	}
	if h.StoreID != snapshot.StoreID || h.BaseGeneration > snapshot.Generation {
		return snapshot, 0, false, codecError()
	}
	state, previous, end := snapshot, h.BaseGeneration, int64(logHeaderSize)
	for {
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(r, header)
		if err == io.EOF && n == 0 {
			if previous < snapshot.Generation {
				return snapshot, end, false, codecError()
			}
			return state, end, false, nil
		}
		if err == io.ErrUnexpectedEOF {
			if previous < snapshot.Generation {
				return snapshot, end, false, codecError()
			}
			return state, end, true, nil
		}
		if err != nil {
			return snapshot, end, false, err
		}
		size := binary.BigEndian.Uint32(header)
		if size > frameLimit || binary.BigEndian.Uint32(header[4:]) > frameLimit {
			return snapshot, end, false, codecError()
		}
		b := make([]byte, int(size))
		if _, err = io.ReadFull(r, b); err == io.EOF || err == io.ErrUnexpectedEOF {
			if previous < snapshot.Generation {
				return snapshot, end, false, codecError()
			}
			return state, end, true, nil
		}
		if err != nil {
			return snapshot, end, false, err
		}
		f, err := decodeFrame(bytes.NewReader(append(header, b...)))
		if err != nil {
			return snapshot, end, false, err
		}
		if f.PreviousGeneration != previous {
			return snapshot, end, false, codecError()
		}
		previous = f.Generation
		if f.Generation > snapshot.Generation {
			state, err = applyStoreFrame(state, f)
			if err != nil {
				return snapshot, end, false, err
			}
		}
		end += int64(frameHeaderSize) + int64(size)
	}
}
