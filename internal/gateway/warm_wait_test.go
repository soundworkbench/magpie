package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// A real cached reply establishes affinity before another request fills
// its lane. The next turn must keep that cache even with an idle mate.
func TestWarmWaitRouting(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		age                                  time.Duration
		hour, off, resting, removed, timeout bool
		wait                                 bool
	}{
		{name: "hot", wait: true},
		{name: "timeout", timeout: true, wait: true},
		{name: "cold", age: 10 * time.Minute},
		{name: "hour", age: 10 * time.Minute, hour: true, wait: true},
		{name: "hour expired", age: 2 * time.Hour, hour: true},
		{name: "off", off: true},
		{name: "resting", resting: true},
		{name: "provider off", removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			if tc.timeout {
				old := warmWaitWindow
				warmWaitWindow = func(int) time.Duration { return 10 * time.Millisecond }
				t.Cleanup(func() { warmWaitWindow = old })
			}
			cfg := settings.Load()
			cfg.WarmWait.Off = tc.off
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			v := &cacheWriter{wrote: 3000}
			if tc.hour {
				v.hour = 3000
			}
			up := httptest.NewServer(v)
			t.Cleanup(up.Close)
			one := 1
			p := provider.Provider{ID: "own", Name: "Own", Key: "k1", Models: []string{"m"}, Anthropic: up.URL, MaxConcurrency: &one}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			mate := p
			mate.ID, mate.Name, mate.Key = "mate", "Mate", "k2"
			if err := provider.Save(mate); err != nil {
				t.Fatal(err)
			}
			if err := provider.SaveGroup(provider.Group{ID: "warm", Name: "Warm", Members: []string{"own/m", "mate/m"}, Routing: provider.Ordered, Affinity: provider.AffinitySession}); err != nil {
				t.Fatal(err)
			}
			s := New()
			const first = `{"model":"group/warm","messages":[{"role":"user","content":"hi"}]}`
			const again = `{"model":"group/warm","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"continue"}]}`
			if code, body := postAs(t, s, "warm-session", first); code != 200 || !strings.Contains(body, "from k1") {
				t.Fatalf("prime: %d %s", code, body)
			}
			sticks.Lock()
			for key, st := range sticks.m {
				st.at = st.at.Add(-tc.age)
				st.hour.at = st.hour.at.Add(-tc.age)
				sticks.m[key] = st
			}
			sticks.Unlock()
			c := candidate{p: p, rest: "own", model: "m"}
			release, ok := s.lanes.tryTake(c.who(), 1)
			if !ok {
				t.Fatal("could not occupy original lane")
			}
			t.Cleanup(release)
			if tc.resting {
				restingUntil.Lock()
				restingUntil.m[c.restKey()] = time.Now().Add(time.Hour)
				restingUntil.note[c.restKey()] = Rest{Why: failOther, Status: 503, Key: c.restKey()}
				restingUntil.Unlock()
			}
			if tc.removed {
				p.Off = true
				if err := provider.Save(p); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(again)).WithContext(ctx)
			req.Header.Set("x-session-id", "warm-session")
			t.Cleanup(func() { cancel(); release(); <-done })
			go func() { defer close(done); s.Handler().ServeHTTP(rec, req) }()
			if tc.wait {
				within(t, "warm wait started", func() bool { return s.warmWait.started.Load() == 1 })
				// Waking the move callback for another lane cannot give
				// away the turn before the warm window has elapsed.
				s.lanes.signal()
				if !tc.timeout {
					release()
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not finish")
			}
			want := "from k2"
			if tc.wait && !tc.timeout {
				want = "from k1"
			}
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("reply: %d %s, want %s", rec.Code, rec.Body.String(), want)
			}
			stats := s.warmWait.snapshot()
			if tc.wait {
				if stats.Started != 1 || stats.Own+stats.GaveWay != 1 || (stats.GaveWay == 1) != tc.timeout {
					t.Fatalf("warm wait stats: %+v", stats)
				}
				route := s.trace.routes[len(s.trace.routes)-1]
				outcome := "own"
				if tc.timeout {
					outcome = "gave-way"
				}
				if !route.Affinity.Warm || len(route.WarmWait) != 1 || route.WarmWait[0].Outcome != outcome {
					t.Fatalf("warm wait absent from route: %+v", route)
				}
				if !tc.timeout && route.Tries[0].WarmWait != route.WarmWait[0].Millis {
					t.Fatalf("warm wait absent from try: %+v", route.Tries[0])
				}
			} else if stats.Started != 0 {
				t.Fatalf("ineligible request waited: %+v", stats)
			}
		})
	}
}

func TestWarmWaitTimeoutAndCancel(t *testing.T) {
	for _, outcome := range []string{"gave-way", "own-after-timeout", "canceled", "queue-ended"} {
		t.Run(outcome, func(t *testing.T) {
			fresh(t)
			s := New()
			one := 1
			c := candidate{p: provider.Provider{ID: "own", MaxConcurrency: &one}, rest: "own", model: "m"}
			m := candidate{p: provider.Provider{ID: "mate", MaxConcurrency: &one}, rest: "mate", model: "m"}
			if outcome == "queue-ended" {
				c.p.QueueWait = 1
			}
			release, _ := s.lanes.tryTake(c.who(), 1)
			t.Cleanup(release)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var hop *WarmWaitHop
			var err error
			var moved candidate
			var movedRelease func()
			delay := 10 * time.Millisecond
			if outcome == "canceled" || outcome == "queue-ended" {
				delay = time.Hour // cancellation and QueueWait win before grace ends
			}
			t.Cleanup(func() { cancel(); release(); <-done })
			go func() {
				defer close(done)
				var rel func()
				rel, _, err, hop = s.takeWarm(ctx, c, delay, "private-session", func() (bool, time.Time) {
					if outcome == "own-after-timeout" {
						// No mate has room at expiry; the original slot
						// becomes free only after that check.
						release()
						return false, time.Time{}
					}
					var next time.Time
					var ok bool
					moved, movedRelease, next, ok = s.waitMate([]candidate{c, m}, 0, true, nil, nil)
					return ok, next
				})
				if rel != nil {
					rel()
				}
				if movedRelease != nil {
					movedRelease()
				}
			}()
			within(t, "warm wait started", func() bool { return s.warmWait.started.Load() == 1 })
			if outcome == "canceled" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("warm queue did not finish")
			}
			if hop == nil || hop.Outcome != outcome {
				t.Fatalf("hop %+v, error %v, want %s", hop, err, outcome)
			}
			stats := s.warmWait.snapshot()
			if stats.Started != 1 || stats.Own+stats.GaveWay+stats.OwnAfterTimeout+stats.Canceled+stats.QueueEnded != 1 || stats.TotalMillis != hop.Millis || stats.MaxMillis != hop.Millis {
				t.Fatalf("stats %+v, hop %+v", stats, hop)
			}
			if outcome == "gave-way" && (!errors.Is(err, errMoved) || moved.who() != m.who() || stats.GaveWay != 1 || hop.Millis < delay.Milliseconds()) {
				t.Fatalf("did not wait before taking mate: %v %+v %+v", err, moved, hop)
			}
			if outcome == "own-after-timeout" && (err != nil || stats.OwnAfterTimeout != 1) {
				t.Fatalf("original lane after timeout: %v %+v", err, stats)
			}
			if s.Lanes()[c.who()].Waiting != 0 {
				t.Fatal("request left in original queue")
			}
			// The public endpoint tells the same counters.
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/magpie/concurrency", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			s.concurrency(rec, req)
			var got struct {
				WarmWait WarmWaitStats `json:"warmWait"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.WarmWait != stats {
				t.Fatalf("concurrency: %d %s (%v)", rec.Code, rec.Body.String(), err)
			}
		})
	}
}

func TestWarmWaitEligibility(t *testing.T) {
	fresh(t)
	for _, why := range []string{"warm", "cold", "not kept", "other answerer", "strict", "off", "reserve", "spent", "tried", "gave way", "rpm", "free"} {
		t.Run(why, func(t *testing.T) {
			s := New()
			one := 1
			c := candidate{p: provider.Provider{ID: "own", MaxConcurrency: &one}, rest: "own", model: "m"}
			a := &Affinity{Last: c.rest, Kept: true, Warm: true, Why: "cache"}
			var order []Weighed
			switch why {
			case "cold":
				a.Warm = false
			case "not kept":
				a.Kept = false
			case "other answerer":
				a.Last = "other"
			case "strict":
				a.Why = "strict"
			case "off":
				c.p.Off = true
			case "reserve":
				c.reserve = 1
			case "spent":
				order = []Weighed{{ID: c.rest, Known: true, Used: 100}}
			case "rpm":
				c.p.MaxRPM = 1
				s.rpms.m = map[string][]time.Time{c.who(): {time.Now()}}
			}
			if why != "free" {
				release, _ := s.lanes.tryTake(c.who(), 1)
				defer release()
			}
			got := s.warmWaitFor(c, a, why == "tried", why == "gave way", order)
			if (got > 0) != (why == "warm") {
				t.Fatalf("warm duration %s for %s", got, why)
			}
		})
	}
}

func TestWarmWaitDoesNotCountWithoutQueueing(t *testing.T) {
	fresh(t)
	s := New()
	one := 1
	c := candidate{p: provider.Provider{ID: "own", MaxConcurrency: &one, QueueLimit: 1}, rest: "own"}
	move := func() (bool, time.Time) {
		t.Error("unqueued request called move")
		return false, time.Time{}
	}
	release, waited, err, hop := s.takeWarm(context.Background(), c, time.Hour, "session", move)
	if err != nil || hop != nil || waited != 0 {
		t.Fatalf("free lane: %v %s %+v", err, waited, hop)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		rel, _, _ := s.lanes.take(ctx, c.who(), 1, 0, 0, nil)
		if rel != nil {
			rel()
		}
	}()
	within(t, "one waiting", func() bool { return s.Lanes()[c.who()].Waiting == 1 })
	_, waited, err, hop = s.takeWarm(ctx, c, time.Hour, "session", move)
	if !errors.Is(err, errQueueFull) || hop != nil || waited != 0 || s.warmWait.started.Load() != 0 {
		t.Fatalf("full queue: %v %s %+v, stats %+v", err, waited, hop, s.warmWait.snapshot())
	}
}
