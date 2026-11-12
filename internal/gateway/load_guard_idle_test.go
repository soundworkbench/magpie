package gateway

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func loadGuardIdleConfig(t *testing.T, cfg settings.LoadGuardConfig) {
	t.Helper()
	s := settings.Load()
	s.LoadGuard = cfg
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

func loadGuardIdleRun(t *testing.T, h *resumeHarness) *subscriptionRun {
	t.Helper()
	b := h.s.subscription
	t.Cleanup(func() {
		b.mu.Lock()
		saved := make([]*savedSession, 0, len(b.shelf))
		for _, s := range b.shelf {
			saved = append(saved, s)
		}
		clear(b.shelf)
		b.mu.Unlock()
		for _, s := range saved {
			s.discard()
		}
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.idle {
		return r
	}
	t.Fatal("no idle Claude run after the reply")
	return nil
}

// Reclaiming a process must leave its resumable session and cache marks,
// so the next request sends only its new turn to Claude Code.
func TestLoadGuardIdleResume(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{IdleProcessMinutes: 30})
	h.ask("rules A", "a1")
	r := loadGuardIdleRun(t, h)
	b := h.s.subscription
	b.mu.Lock()
	key, at, generation := r.idleKey, r.idleAt, r.idleGeneration
	b.mu.Unlock()
	prefix := cachePrefix{count: 2, floor: "5m", ttl: "5m"}
	r.cachePrefix = prefix
	r.mu.Lock()
	sid := r.sessionID
	r.mu.Unlock()
	b.expireIdle(r, generation, at.Add(30*time.Minute))
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if !closed {
		t.Fatal("idle timeout left the Claude process running")
	}
	select {
	case <-r.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("reclaimed Claude process did not exit")
	}
	b.mu.Lock()
	saved := b.shelf[key]
	b.mu.Unlock()
	if saved == nil || saved.session != sid || saved.cachePrefix != prefix || !saved.at.Equal(at) {
		t.Fatalf("idle reclaim lost the session, cache prefix or original deadline: %+v", saved)
	}
	if !saved.saved() {
		t.Fatal("reclaimed session file was removed")
	}
	before := h.runs()
	h.ask("rules A", "a1", "a2")
	var resumed string
	for pid, log := range h.runs() {
		if _, old := before[pid]; !old {
			resumed = log
		}
	}
	if !strings.Contains(resumed, "--resume "+sid) {
		t.Fatalf("next turn did not resume reclaimed session: %s", resumed)
	}
	_, told, ok := strings.Cut(resumed, "\ntold ")
	if !ok || !strings.Contains(told, "a2") || strings.Contains(told, "a1") {
		t.Fatalf("resumed process replayed the conversation: %s", resumed)
	}
	if got := loadGuardIdleRun(t, h).cachePrefix; got != prefix {
		t.Fatalf("resumed cache prefix = %+v, want %+v", got, prefix)
	}
}

// Legacy lifetime applies when disabled or no shorter interval was chosen;
// changing settings while a guard waits must be honored when it fires.
func TestLoadGuardIdleHotSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  settings.LoadGuardConfig
	}{
		{"off", settings.LoadGuardConfig{Off: true, IdleProcessMinutes: 30}},
		{"hour", settings.LoadGuardConfig{IdleProcessMinutes: 60}},
		{"long", settings.LoadGuardConfig{IdleProcessMinutes: 90}},
		{"largest", settings.LoadGuardConfig{IdleProcessMinutes: int(^uint(0) >> 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResumeHarness(t)
			loadGuardIdleConfig(t, tc.cfg)
			h.ask("rules", "first")
			r := loadGuardIdleRun(t, h)
			b := h.s.subscription
			b.mu.Lock()
			timer, at, generation := r.idleGuard, r.idleAt, r.idleGeneration
			b.mu.Unlock()
			if timer != nil {
				t.Fatal("legacy lifetime installed an early reclaim timer")
			}
			r.mu.Lock()
			ends := r.ends
			r.mu.Unlock()
			if ends.Sub(at) < 59*time.Minute || ends.Sub(at) > 61*time.Minute {
				t.Fatalf("legacy lifetime ends at %v after idle", ends.Sub(at))
			}
			loadGuardIdleConfig(t, settings.LoadGuardConfig{IdleProcessMinutes: 30})
			r.endIdle()
			b.mu.Lock()
			timer = r.idleGuard
			b.mu.Unlock()
			if timer == nil {
				t.Fatal("hot enabled idle guard did not schedule reclaim")
			}
			loadGuardIdleConfig(t, tc.cfg)
			b.expireIdle(r, generation, at.Add(30*time.Minute))
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				t.Fatal("idle guard killed a run after the setting disabled early reclaim")
			}
			h.ask("rules", "first", "next")
			if len(h.runs()) != 1 {
				t.Fatal("disabling reclaim did not preserve the live process")
			}
		})
	}
}

// Duplicate expiry and stale expiry from a previous idle period must not
// replace the shelf or claim a process already serving its next turn.
func TestLoadGuardIdleStaleExpiry(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{IdleProcessMinutes: 30})
	h.ask("rules", "first")
	r := loadGuardIdleRun(t, h)
	b := h.s.subscription
	b.mu.Lock()
	old, at := r.idleGeneration, r.idleAt
	b.mu.Unlock()
	h.ask("rules", "first", "next")
	b.expireIdle(r, old, at.Add(time.Hour))
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		t.Fatal("stale expiry killed a reused and newly idled run")
	}
	b.mu.Lock()
	generation, at, key := r.idleGeneration, r.idleAt, r.idleKey
	b.mu.Unlock()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { b.expireIdle(r, generation, at.Add(30*time.Minute)) })
	}
	wg.Wait()
	b.mu.Lock()
	saved := b.shelf[key]
	n := len(b.shelf)
	b.mu.Unlock()
	if saved == nil || n != 1 || !saved.saved() {
		t.Fatal("concurrent expiry did not preserve exactly one session")
	}
	b.expireIdle(r, generation, at.Add(31*time.Minute))
	b.mu.Lock()
	same := b.shelf[key] == saved
	b.mu.Unlock()
	if !same || !saved.saved() {
		t.Fatal("duplicate expiry replaced or discarded the saved session")
	}
	if b.keepIdle(r, key, "") {
		t.Fatal("a claimed run was returned to the idle table")
	}
}

func TestLoadGuardIdleExpiryRacesNextTurn(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{})
	h.ask("rules", "first")
	r := loadGuardIdleRun(t, h)
	b := h.s.subscription
	b.mu.Lock()
	generation, at := r.idleGeneration, r.idleAt
	b.mu.Unlock()
	start, done := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); <-start; b.expireIdle(r, generation, at.Add(30*time.Minute)) }()
	close(start)
	h.ask("rules", "first", "next")
	<-done
	next := loadGuardIdleRun(t, h)
	next.mu.Lock()
	closed := next.closed
	next.mu.Unlock()
	if closed {
		t.Fatal("expiry ended the next turn's run")
	}
	if next == r {
		b.mu.Lock()
		saved := len(b.shelf)
		b.mu.Unlock()
		if saved != 0 {
			t.Fatal("a reused run was also shelved")
		}
	}
}

func TestLoadGuardIdleDoesNotClaimTakenRun(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{})
	h.ask("rules", "first")
	r := loadGuardIdleRun(t, h)
	b := h.s.subscription
	b.mu.Lock()
	key, generation, at := r.idleKey, r.idleGeneration, r.idleAt
	// The exact table transition made by resume, while an expiry is queued.
	delete(b.idle, key)
	r.idleKey = ""
	b.mu.Unlock()
	b.expireIdle(r, generation, at.Add(30*time.Minute))
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		t.Fatal("expiry claimed a run already taken by the next turn")
	}
	b.mu.Lock()
	if len(b.shelf) != 0 {
		b.mu.Unlock()
		t.Fatal("taken run was shelved")
	}
	b.idle[key] = r
	r.idleKey = key
	b.mu.Unlock()
	h.ask("rules", "first", "next")
	if len(h.runs()) != 1 {
		t.Fatal("taken run could not answer on")
	}
}

// The short timer is real. Only the idle duration is injected; a generous
// deadline waits for process exit, not for a scheduler-dependent sleep.
func TestLoadGuardIdleTimer(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{})
	b := h.s.subscription
	var duration atomic.Int64
	duration.Store(int64(idleLongest))
	b.idleDuration = func() time.Duration { return time.Duration(duration.Load()) }
	h.ask("rules", "first")
	r := loadGuardIdleRun(t, h)
	r.mu.Lock()
	sid := r.sessionID
	r.mu.Unlock()
	b.mu.Lock()
	key := r.idleKey
	r.idleAt = time.Now()
	at := r.idleAt
	b.mu.Unlock()
	duration.Store(int64(40 * time.Millisecond))
	r.endIdle()
	select {
	case <-r.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("endIdle timer did not end the idle Claude process")
	}
	b.mu.Lock()
	saved, idle := b.shelf[key], b.idle[key]
	b.mu.Unlock()
	if idle != nil || saved == nil || saved.session != sid || !saved.at.Equal(at) || !saved.saved() {
		t.Fatalf("timer did not shelve the ended process: idle=%v saved=%+v", idle != nil, saved)
	}
}
