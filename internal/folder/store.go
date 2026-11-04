package folder

import (
	"fmt"
	"io"
	"os"
)

// indexStore is serialized by its owner's saveMu (after scanMu). An I/O error
// poisons the writer until restart: a failed flush may still have reached disk.
type indexStore struct {
	log    *os.File
	state  storeSnapshot
	failed error
	write  func([]byte) (int, error)
	flush  func(*os.File) error
}

// openIndexStore requires an already durable snapshot and log header. Creation
// and replacement belong to migration/compaction; never create a missing log here.
func openIndexStore(path string, snapshot storeSnapshot) (*indexStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	state, end, torn, err := replayStore(f, snapshot)
	if err == nil && torn {
		err = f.Truncate(end)
		if err == nil {
			err = syncFile(f)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return &indexStore{log: f, state: state, write: f.Write, flush: syncFile}, nil
}
func (s *indexStore) close() error { return s.log.Close() }
func (s *indexStore) append(frame storeFrame) error {
	if s.failed != nil {
		return s.failed
	}
	next, err := applyStoreFrame(s.state, frame)
	if err != nil {
		return err
	}
	b, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	n, err := s.write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.flush(s.log)
	}
	if err != nil {
		s.failed = fmt.Errorf("store append requires recovery: %w", err)
		return s.failed
	}
	s.state = next // Publish only after the durable boundary.
	return nil
}
