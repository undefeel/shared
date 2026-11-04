package folder

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func TestPersistenceMeasurement(t *testing.T) {
	var m PersistenceMeasurement
	h := persistHooks{probe: m.Probe()}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.write("log", "", io.Discard, []byte("abc"))
			h.phase("tx", "G5")
			m.Snapshot()
		}()
	}
	wg.Wait()
	b, g := m.Snapshot()
	if b["log"] != 96 || g != 32 {
		t.Fatalf("%v %d", b, g)
	}
}

func TestPerfReadyQueueGroupSize(t *testing.T) {
	total, groups := 0, 0
	c := &groupCoordinator{changed: make(chan struct{}), stopped: make(chan struct{}), commit: func(rs []*groupRequest) error { total += len(rs); groups++; return nil }}
	for i := 0; i < 128; i++ {
		c.pending = append(c.pending, &groupRequest{Context: context.Background(), Names: []string{fmt.Sprint(i)}, Bytes: 11264, ready: time.Now(), done: make(chan error, 1)})
		c.count++
		c.bytes += 11264
	}
	go c.run()
	c.close(false)
	if groups == 0 || total != 128 || float64(total)/float64(groups) < 32 {
		t.Fatalf("total=%d groups=%d", total, groups)
	}
}
