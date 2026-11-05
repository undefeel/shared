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
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for cnt := 0; cnt < 100; cnt++ {
				kind := "in"
				if cnt%2 == 1 {
					kind = "out"
				}
				act.add(Event{Time: time.Now(), Kind: kind, Name: fmt.Sprintf("file_%d_%d.txt", id, cnt), Peer: fmt.Sprintf("peer_%d", id), Size: int64(cnt * 100)})
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			act.flush()
		}
	}()
	close(start)
	wg.Wait()
}
