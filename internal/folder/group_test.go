package folder

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestGroupLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		bytes     int64
		duplicate bool
		want      int
	}{
		{"count", 65, 1, false, 64}, {"bytes", 3, 4 << 20, false, 2}, {"large", 2, 9 << 20, false, 1}, {"duplicate", 2, 1, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batches := make(chan int, tc.count)
			c := &groupCoordinator{changed: make(chan struct{}), stopped: make(chan struct{}), commit: func(rs []*groupRequest) error { batches <- len(rs); return nil }}
			for i := 0; i < tc.count; i++ {
				name := fmt.Sprint(i)
				if tc.duplicate {
					name = "same"
				}
				c.pending = append(c.pending, &groupRequest{Context: context.Background(), Names: []string{name}, Bytes: tc.bytes, ready: time.Now(), done: make(chan error, 1)})
				c.count++
				c.bytes += tc.bytes
			}
			go c.run()
			c.close(false)
			if got := <-batches; got != tc.want {
				t.Fatalf("first batch %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGroupTimerAndDrain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := newGroupCoordinator(func(rs []*groupRequest) error { close(entered); <-release; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	start := time.Now()
	go func() { result <- c.submit(ctx, []string{"a"}, 1, 1) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timer did not dispatch")
	}
	if time.Since(start) < groupDelay {
		t.Fatal("dispatched before collection deadline")
	}
	// Model a durable intent: cancellation and close cannot release the caller
	// or stop the commit. Publication is represented by releasing the callback.
	cancel()
	drained := make(chan struct{})
	go func() { c.close(true); close(drained) }()
	select {
	case <-result:
		t.Fatal("returned before durable publication")
	case <-drained:
		t.Fatal("did not drain")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	<-drained
	if err := c.submit(context.Background(), []string{"b"}, 1, 1); err != errGroupClosed {
		t.Fatal(err)
	}
}

func TestGroupReservations(t *testing.T) {
	for _, pair := range [][2]string{{"a", "a/b"}, {"a/b", "a"}, {"x", "x"}} {
		if !groupOverlap(pair[0], pair[1]) {
			t.Fatal(pair)
		}
	}
	if groupOverlap("a", "ab") {
		t.Fatal("unrelated names overlap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newGroupCoordinator(func([]*groupRequest) error { t.Error("canceled work committed"); return nil })
	if err := c.submit(ctx, []string{"a"}, 1, 1); err != context.Canceled {
		t.Fatal(err)
	}
	c.close(false)
}
