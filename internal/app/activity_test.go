package app

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestActivityRace(t *testing.T) {
	dir := t.TempDir()
	actPath := filepath.Join(dir, "activity.json")
	act := loadActivity(actPath)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 10 goroutines calling add
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cnt := 0
			for {
				select {
				case <-stop:
					return
				default:
					kind := "in"
					if cnt%2 == 1 {
						kind = "out"
					}
					act.add(Event{
						Time: time.Now(),
						Kind: kind,
						Name: fmt.Sprintf("file_%d_%d.txt", id, cnt),
						Peer: fmt.Sprintf("peer_%d", id),
						Size: int64(cnt * 100),
					})
					cnt++
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(i)
	}

	// parallel flusher
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				act.flush()
				return
			default:
				act.flush()
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
