package gateway

import (
	"log"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// idleProcessFor caps the configured minutes before converting to a
// duration, so even a very large setting keeps the legacy lifetime.
func idleProcessFor() time.Duration {
	g := settings.LoadGuardNow()
	if g.Off || g.IdleProcessMinutes >= int(idleLongest/time.Minute) {
		return idleLongest
	}
	return time.Duration(g.IdleProcessMinutes) * time.Minute
}

func (b *subscriptionBridge) idleProcessFor() time.Duration {
	if b.idleDuration != nil {
		return b.idleDuration()
	}
	return idleProcessFor()
}

// stopIdleGuard is called under the bridge lock. An already firing
// callback must also match the idle table and generation before claiming it.
func (r *subscriptionRun) stopIdleGuard() {
	if r.idleGuard != nil {
		r.idleGuard.Stop()
		r.idleGuard = nil
	}
}

func (r *subscriptionRun) endIdle() {
	b := r.bridge
	d := b.idleProcessFor()
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.idleReclaimed || r.idleKey == "" || b.idle[r.idleKey] != r {
		return
	}
	r.stopIdleGuard()
	// Keep the old timer as the final deadline. Early expiry has its own
	// callback, which can shelve under the same lock as the next turn takes.
	r.endIn(idleLongest)
	if d >= idleLongest {
		return
	}
	generation := r.idleGeneration
	r.mu.Lock()
	r.ends = r.idleAt.Add(d)
	r.mu.Unlock()
	r.idleGuard = time.AfterFunc(max(time.Until(r.idleAt.Add(d)), 0), func() { b.expireIdle(r, generation, time.Now()) })
}

func (b *subscriptionBridge) expireIdle(r *subscriptionRun, generation uint64, now time.Time) {
	d := b.idleProcessFor()
	b.mu.Lock()
	if r.idleReclaimed || r.idleGeneration != generation || r.idleKey == "" || b.idle[r.idleKey] != r {
		b.mu.Unlock()
		return
	}
	r.stopIdleGuard()
	if d >= idleLongest {
		r.mu.Lock()
		r.ends = r.idleAt.Add(idleLongest)
		r.mu.Unlock()
		b.mu.Unlock()
		return
	}
	if left := r.idleAt.Add(d).Sub(now); left > 0 {
		r.mu.Lock()
		r.ends = r.idleAt.Add(d)
		r.mu.Unlock()
		r.idleGuard = time.AfterFunc(left, func() { b.expireIdle(r, generation, time.Now()) })
		b.mu.Unlock()
		return
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		b.mu.Unlock()
		return
	}
	// Claim exactly once, before releasing the lock. A next turn either
	// already took the live run, or finds its saved session after this.
	r.idleReclaimed = true
	b.shelve(r)
	delete(b.idle, r.idleKey)
	// Keep idleKey until removal so a branch cannot mistake this run for
	// an active conversation between shelving and aborting it.
	b.mu.Unlock()
	log.Printf("claude: idle reclaim of Claude Code pid %d (%s) after %s; its next turn resumes its saved session", r.pid(), r.model, d)
	r.abortFor("idle reclaim after " + d.String())
}
