package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

type warmWaitCounters struct {
	started, own, gaveWay, ownAfterTimeout, canceled, queueEnded atomic.Int64
	totalMillis, maxMillis                                       atomic.Int64
}

// WarmWaitStats counts actual lane waits, not a full queue's refusals or
// requests whose slot became free before they could join the queue.
type WarmWaitStats struct {
	Started         int64 `json:"started"`
	Own             int64 `json:"own"`
	GaveWay         int64 `json:"gaveWay"`
	OwnAfterTimeout int64 `json:"ownAfterTimeout"`
	Canceled        int64 `json:"canceled"`
	QueueEnded      int64 `json:"queueEnded"`
	TotalMillis     int64 `json:"totalMillis"`
	MaxMillis       int64 `json:"maxMillis"`
}

func (w *warmWaitCounters) snapshot() WarmWaitStats {
	return WarmWaitStats{
		Started: w.started.Load(), Own: w.own.Load(), GaveWay: w.gaveWay.Load(),
		OwnAfterTimeout: w.ownAfterTimeout.Load(), Canceled: w.canceled.Load(),
		QueueEnded: w.queueEnded.Load(), TotalMillis: w.totalMillis.Load(), MaxMillis: w.maxMillis.Load(),
	}
}

// WarmWaitHop survives removal of a try that gave way before sending
// anything, so the route still explains where its queue time was spent.
type WarmWaitHop struct {
	Who     string `json:"who"`
	Millis  int64  `json:"ms"`
	Outcome string `json:"outcome"`
}

// Tests shorten the window without weakening the setting's lower bound.
var warmWaitWindow = func(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// warmWaitFor applies only to congestion on the last answerer's lane.
// Affinity alone is insufficient: session and turn affinity can outlive
// the cache, and retries must be free to leave an account that failed.
func (s *Server) warmWaitFor(c candidate, a *Affinity, tried, gave bool, order []Weighed) time.Duration {
	if a == nil || !a.Kept || !a.Warm || a.Why == "strict" ||
		(a.Last != c.rest && a.Last != c.who()) || tried || gave || c.p.Off || c.reserve > 0 {
		return 0
	}
	w := settings.WarmWaitNow()
	if w.Off {
		return 0
	}
	if _, resting := restedUntil(c); resting {
		return 0
	}
	for _, seat := range order {
		if seat.ID == c.rest && seat.Known && seat.Used >= provider.SpentShareOf(c.p.Routing) && !seat.CreditsFirst {
			return 0
		}
	}
	if s.lanes.free(c.who(), c.p.LaneLimit()) || !s.rpms.free(c.who(), c.p.RPMLimit()) {
		return 0
	}
	return warmWaitWindow(w.Seconds)
}

// takeWarm uses the same queue and cancellation rules as take. Its window
// starts at the lane's queue timestamp, so a gateway key's wait cannot
// consume the cache's grace period. Passing a duration keeps short queue
// tests independent of the setting's production minimum.
func (s *Server) takeWarm(ctx context.Context, c candidate, delay time.Duration, session string, move func() (bool, time.Time)) (func(), time.Duration, error, *WarmWaitHop) {
	var deadline time.Time
	var queuedAt func(time.Time)
	guarded := move
	if delay > 0 {
		queuedAt = func(at time.Time) {
			deadline = at.Add(delay)
			s.warmWait.started.Add(1)
		}
		guarded = func() (bool, time.Time) {
			if time.Now().Before(deadline) {
				return false, deadline
			}
			return move()
		}
	}
	release, waited, err := s.lanes.takeQueued(ctx, c.who(), c.p.LaneLimit(), c.p.QueueLimit, time.Duration(c.p.QueueWait)*time.Second, guarded, queuedAt)
	if deadline.IsZero() {
		return release, waited, err, nil
	}
	outcome := "own"
	switch {
	case errors.Is(err, errMoved):
		outcome = "gave-way"
		s.warmWait.gaveWay.Add(1)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		outcome = "canceled"
		s.warmWait.canceled.Add(1)
	case err != nil:
		// QueueWait still ends the wait; it is neither a client cancel
		// nor a successful acquisition of the original lane.
		outcome = "queue-ended"
		s.warmWait.queueEnded.Add(1)
	case waited >= delay:
		outcome = "own-after-timeout"
		s.warmWait.ownAfterTimeout.Add(1)
	default:
		s.warmWait.own.Add(1)
	}
	ms := waited.Milliseconds()
	s.warmWait.totalMillis.Add(ms)
	for old := s.warmWait.maxMillis.Load(); ms > old; old = s.warmWait.maxMillis.Load() {
		if s.warmWait.maxMillis.CompareAndSwap(old, ms) {
			break
		}
	}
	hash := sha256.Sum256([]byte(session))
	log.Printf("warm-wait: %s waited=%dms outcome=%s session=%x", c.who(), ms, outcome, hash[:4])
	return release, waited, err, &WarmWaitHop{Who: c.who(), Millis: ms, Outcome: outcome}
}
