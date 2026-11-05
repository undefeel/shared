package app

import (
	"log"
	"time"

	"sharedir/internal/identity"
)

type reconnectEpisode struct {
	first, last, emitted time.Time
	pending              int
}
type reconnectLog struct {
	logger   *log.Logger
	peer     string
	episodes map[string]*reconnectEpisode
}

func newReconnectLog(logger *log.Logger, id string) *reconnectLog {
	peer := identity.ShortID(id)
	for _, c := range peer {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			peer = "unknown"
			break
		}
	}
	if peer == "" {
		peer = "unknown"
	}
	return &reconnectLog{logger: logger, peer: peer, episodes: make(map[string]*reconnectEpisode)}
}

// pending counts failures since the previous emission, excluding the first
// failure, which is emitted immediately.
func (r *reconnectLog) failure(now time.Time, code string) {
	switch code {
	case "network", "offline", "protocol_incompatible":
	default:
		return
	}
	e := r.episodes[code]
	if e == nil {
		r.episodes[code] = &reconnectEpisode{first: now, last: now, emitted: now}
		r.logger.Printf("%s: reconnect failure code=%s count=1", r.peer, code)
		return
	}
	e.last = now
	e.pending++
	if now.Sub(e.emitted) >= 5*time.Minute {
		r.logger.Printf("%s: reconnect failure code=%s count=%d last=%s", r.peer, code, e.pending, e.last.UTC().Format(time.RFC3339Nano))
		e.pending = 0
		e.emitted = now
	}
}
func (r *reconnectLog) recovered(now time.Time) {
	if len(r.episodes) == 0 {
		return
	}
	for _, code := range []string{"network", "offline", "protocol_incompatible"} {
		if e := r.episodes[code]; e != nil && e.pending > 0 {
			r.logger.Printf("%s: reconnect failure code=%s count=%d last=%s", r.peer, code, e.pending, e.last.UTC().Format(time.RFC3339Nano))
		}
	}
	r.logger.Printf("%s: connection recovered", r.peer)
	clear(r.episodes)
}
