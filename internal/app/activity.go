package app

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

const (
	keepDays   = 30
	keepEvents = 50
)

// Event — запись в ленте недавних изменений.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // in, out, deleted, conflict
	Name string    `json:"name"`
	Peer string    `json:"peer"`
	Size int64     `json:"size,omitempty"`
}

// Day — сколько байт пришло и ушло за день (локальная дата).
type Day struct {
	Date string `json:"date"` // 2006-01-02
	In   int64  `json:"in"`
	Out  int64  `json:"out"`
}

// activity копит статистику передачи и ленту событий и переживает рестарт.
type activity struct {
	path string

	mu     sync.Mutex
	days   map[string]*Day
	recent []Event
	dirty  bool
}

type activityFile struct {
	Days   []*Day  `json:"days"`
	Recent []Event `json:"recent"`
}

func loadActivity(path string) *activity {
	a := &activity{path: path, days: map[string]*Day{}}
	if data, err := os.ReadFile(path); err == nil {
		var f activityFile
		if json.Unmarshal(data, &f) == nil {
			for _, d := range f.Days {
				a.days[d.Date] = d
			}
			a.recent = f.Recent
		}
	}
	return a
}

func (a *activity) add(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ev.Kind == "in" || ev.Kind == "out" {
		date := ev.Time.Format(time.DateOnly)
		d := a.days[date]
		if d == nil {
			d = &Day{Date: date}
			a.days[date] = d
		}
		if ev.Kind == "in" {
			d.In += ev.Size
		} else {
			d.Out += ev.Size
		}
	}
	a.recent = append([]Event{ev}, a.recent...)
	if len(a.recent) > keepEvents {
		a.recent = a.recent[:keepEvents]
	}
	a.dirty = true
}

// snapshot возвращает последние n дней (включая пустые) и до limit событий.
func (a *activity) snapshot(n, limit int) ([]Day, []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	days := make([]Day, 0, n)
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).Format(time.DateOnly)
		if d := a.days[date]; d != nil {
			days = append(days, *d)
		} else {
			days = append(days, Day{Date: date})
		}
	}
	events := append([]Event(nil), a.recent[:min(limit, len(a.recent))]...)
	return days, events
}

// flush сохраняет журнал, если он менялся, и чистит дни старше keepDays.
func (a *activity) flush() {
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format(time.DateOnly)
	f := activityFile{Recent: a.recent}
	for date, d := range a.days {
		if date < cutoff {
			delete(a.days, date)
			continue
		}
		f.Days = append(f.Days, d)
	}
	a.dirty = false
	a.mu.Unlock()
	if data, err := json.Marshal(f); err == nil {
		tmp := a.path + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			os.Rename(tmp, a.path)
		}
	}
}
