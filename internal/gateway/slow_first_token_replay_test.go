package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// replayNoFirst has sent headers but no content. The gateway therefore has
// something to read while the group's first-token timer decides whether to
// move on.
type replayNoFirst struct {
	mu sync.Mutex
	n  int
}

func (h *replayNoFirst) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.ReadAll(r.Body)
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

type replayStall struct {
	done chan struct{}
}

func (h *replayStall) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `data: {"id":"stall","choices":[{"index":0,"delta":{"role":"assistant","content":"from stall"}}]}`+"\n\n")
	w.(http.Flusher).Flush()
	<-r.Context().Done()
	close(h.done)
}

// Three members reproduce several accounts being slow at once: the first
// two are abandoned at the group's deadline and the third answer reaches the
// client once, with both slow attempts retained in the route trace.
func TestSlowFirstTokenReplayMovesThroughSeveralSlowMembers(t *testing.T) {
	fresh(t)
	a, b := &replayNoFirst{}, &replayNoFirst{}
	slowOn(t, "a", a)
	slowOn(t, "b", b)
	scriptedOn(t, "c", provider.Chat, &scripted{replies: []reply{{200, "text/event-stream", chatStreamOK}}})
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m", "c/m"}, Routing: provider.Ordered, FirstToken: 2}); err != nil {
		t.Fatal(err)
	}

	s := New()
	code, body := postAs(t, s, "", `{"model":"group/g","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || strings.Count(body, "from b") != 1 {
		t.Fatalf("%d %q", code, body)
	}
	r := s.trace.routes[len(s.trace.routes)-1]
	if len(r.Tries) != 3 || r.Tries[0].Fail != failSlow || r.Tries[1].Fail != failSlow || r.Tries[2].Status != http.StatusOK {
		t.Fatalf("tries: %+v", r.Tries)
	}
}

// Once content has reached the client, a later upstream stall is allowed to
// finish in place. The next group member is not asked and the partial stream
// is never duplicated.
func TestSlowFirstTokenReplayDoesNotRetryAfterOutput(t *testing.T) {
	fresh(t)
	stall := &replayStall{done: make(chan struct{})}
	slowOn(t, "a", stall)
	b := &scripted{replies: []reply{{200, "text/event-stream", chatStreamOK}}}
	scriptedOn(t, "b", provider.Chat, b)
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered, FirstToken: 1}); err != nil {
		t.Fatal(err)
	}

	s := New()
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"group/g","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	var got strings.Builder
	for !strings.Contains(got.String(), "from stall") {
		line, readErr := reader.ReadString('\n')
		got.WriteString(line)
		if readErr != nil {
			resp.Body.Close()
			cancel()
			t.Fatalf("stream ended before output: %v (%q)", readErr, got.String())
		}
	}
	cancel()
	resp.Body.Close()
	select {
	case <-stall.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled upstream was not canceled")
	}
	within(t, "route after canceled stream", func() bool {
		s.trace.mu.Lock()
		defer s.trace.mu.Unlock()
		return len(s.trace.routes) > 0 && s.trace.routes[len(s.trace.routes)-1].Done && len(s.trace.routes[len(s.trace.routes)-1].Tries) > 0
	})
	s.trace.mu.Lock()
	r := *s.trace.routes[len(s.trace.routes)-1]
	s.trace.mu.Unlock()
	if len(r.Tries) != 1 || r.Tries[0].Fail == failSlow || b.n != 0 || strings.Count(got.String(), "from stall") != 1 {
		t.Fatalf("tries: %+v, body %q, fallback calls %d", r.Tries, got.String(), b.n)
	}
}

// The configurable policy starts resting on the third slow start, then lets
// the member back in as soon as its configured rest has expired. The normal
// policy is covered by TestSlowStartsRestTheModelOnItsAccount (two starts).
func TestSlowFirstTokenReplayUsesConfiguredRestPolicy(t *testing.T) {
	fresh(t)
	st := settings.Load()
	st.SlowForget, st.SlowRests, st.SlowCooldown = 600, 3, 900
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	up := &keyedSlow{key: "k1", model: "m"}
	up.slow.Store(true)
	serveOn(t, "a", "k1", []string{"m"}, up, "k2")
	if err := provider.SetRestCap("a", 20); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m"}, Routing: provider.Ordered, FirstToken: 1}); err != nil {
		t.Fatal(err)
	}
	s := New()
	ask := func() Try {
		t.Helper()
		up.reset()
		code, body := postAs(t, s, "", `{"model":"group/g","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK || !strings.Contains(body, "from b") {
			t.Fatalf("%d %s", code, body)
		}
		return s.trace.routes[len(s.trace.routes)-1].Tries[0]
	}
	if tr := ask(); tr.Rest != nil {
		t.Fatalf("first slow start rested: %+v", tr)
	}
	if tr := ask(); tr.Rest != nil {
		t.Fatalf("second slow start rested: %+v", tr)
	}
	tr := ask()
	if tr.Rest == nil || tr.Rest.Failures != 3 || tr.Rest.By != "cooldown" {
		t.Fatalf("third slow start: %+v", tr.Rest)
	}
	if d := time.Until(tr.Rest.Until); d < 895*time.Second || d > 900*time.Second {
		t.Fatalf("configured rest %s, want 15m", d)
	}

	// Expire only the rest; the next request can select the member again and
	// answers promptly, proving the rest is time based rather than permanent.
	k1m := "a#" + provider.KeyID("k1") + "/m"
	restingUntil.Lock()
	restingUntil.m[k1m] = time.Now().Add(-time.Second)
	restingUntil.Unlock()
	routed.Lock()
	routed.slow[k1m] = slowRun{n: 3, at: time.Now().Add(-601 * time.Second)}
	routed.Unlock()
	// An old run is forgotten before the next slow start, so it does not
	// immediately create another rest.
	up.slow.Store(true)
	if tr := ask(); tr.Rest != nil {
		t.Fatalf("slow start outside the window rested: %+v", tr)
	}
	up.slow.Store(false)
	if tr := ask(); up.first() != "k1/m" || tr.Status != http.StatusOK {
		t.Fatalf("after rest: first %q, try %+v", up.first(), tr)
	}
}

// Without a provider rest cap, the gateway's own ten-minute cap still limits
// a configured first cooldown of fifteen minutes.
func TestSlowFirstTokenReplayUsesDefaultRestCap(t *testing.T) {
	fresh(t)
	st := settings.Load()
	st.SlowForget, st.SlowRests, st.SlowCooldown = 600, 3, 900
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	up := &keyedSlow{key: "k1", model: "m"}
	up.slow.Store(true)
	serveOn(t, "a", "k1", []string{"m"}, up, "k2")
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m"}, Routing: provider.Ordered, FirstToken: 1}); err != nil {
		t.Fatal(err)
	}
	s := New()
	for i := 0; i < 3; i++ {
		up.reset()
		code, body := postAs(t, s, "", `{"model":"group/g","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK || !strings.Contains(body, "from b") {
			t.Fatalf("slow start %d: %d %s", i+1, code, body)
		}
	}
	r := s.trace.routes[len(s.trace.routes)-1].Tries[0]
	if r.Rest == nil || r.Rest.Failures != 3 {
		t.Fatalf("third slow start: %+v", r.Rest)
	}
	if d := time.Until(r.Rest.Until); d <= 9*time.Minute || d > 10*time.Minute {
		t.Fatalf("default cap rest %s, want at most 10m and positive", d)
	}
}
