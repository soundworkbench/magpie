package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// A large first request stays upstream while the next two settle. Its
// reservation exceeds the remaining dollars, but only settled use blocks.
func TestKeyLimitActualOnlyParallel(t *testing.T) {
	for _, content := range []string{"image", "context", "models"} {
		t.Run(content, func(t *testing.T) {
			fresh(t)
			budget.Forget()
			t.Cleanup(budget.Forget)
			now := pinLimitClock(t)
			started, finish := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(finish) }) }
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					close(started)
					<-finish
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`)
			}))
			t.Cleanup(up.Close)
			t.Cleanup(unblock)
			if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "k", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			input, zero := 60_000.0, 0.0
			if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
				t.Fatal(err)
			}
			keys, secrets := newCaller(t, "Capped")
			lim := &access.Limit{Period: "day", Cost: 30}
			model := "plan/m1"
			if content == "models" {
				lim.Models = []string{"plan/m1"}
				if err := provider.SaveGroup(provider.Group{Name: "Solo", Members: []string{"plan/m1"}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
				model = "group/solo"
			}
			setLimit(t, keys[0].ID, lim)
			budget.Append(usage.Record{Time: now, CallerKeyID: keys[0].ID, Provider: "plan", Model: "m1", Input: 300, Status: 200})
			body := `{"model":"plan/m1","messages":[{"role":"user","content":"` + strings.Repeat("x", 800<<10) + `"}]}`
			if content == "image" {
				body = `{"model":"plan/m1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 2<<20) + `"}}]}]}`
			}
			h := New().Front()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- keyCall(h, secrets[0], "/v1/chat/completions", body) }()
			t.Cleanup(func() {
				unblock()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("first request did not return")
				}
			})
			select {
			case <-started:
			case w := <-done:
				done <- w
				t.Fatalf("first request: %d %s", w.Code, w.Body.String())
			case <-time.After(10 * time.Second):
				t.Fatal("upstream not reached")
			}
			status := func() *budget.Status { return budget.Of(access.Key{ID: keys[0].ID, Limit: lim}, now) }
			if st := status(); st.InFlight != 1 || st.Reserved == 0 || st.ReservedCost <= 12 || st.Cost != 18 {
				t.Fatalf("held status %+v", st)
			}
			for i := 2; i <= 3; i++ {
				next := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
				if content != "models" {
					next = body
				}
				w := keyCall(h, secrets[0], "/v1/chat/completions", next)
				if w.Code != 200 {
					t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			w := keyCall(h, secrets[0], "/v1/chat/completions", chatReq)
			_, reset := lim.Window(now)
			if w.Code != 429 || w.Header().Get("Retry-After") != strconv.Itoa(int(reset.Sub(now).Seconds())) || strings.Contains(w.Body.String(), "in flight") {
				t.Fatalf("settled refusal: %d %s %v", w.Code, w.Body.String(), w.Header())
			}
			unblock()
			select {
			case w := <-done:
				// Leave the completion for cleanup too, including on a failed assertion.
				done <- w
				if w.Code != 200 {
					t.Fatalf("first response: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("first request did not finish")
			}
			if st := status(); st.InFlight != 0 || st.Reserved != 0 || st.ReservedCost != 0 || st.Cost != 36 {
				t.Fatalf("settled status %+v", st)
			}
			if calls.Load() != 3 {
				t.Fatalf("upstream calls %d", calls.Load())
			}
		})
	}
}

// Exercise release through the real HTTP server, including a client closing
// its stream after receiving content while the upstream is still running.
func TestKeyLimitActualOnlyReleases(t *testing.T) {
	for _, end := range []string{"stream", "failure", "disconnect"} {
		t.Run(end, func(t *testing.T) {
			fresh(t)
			budget.Forget()
			t.Cleanup(budget.Forget)
			now := pinLimitClock(t)
			finish := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(finish) }) }
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if end == "failure" {
					w.WriteHeader(400)
					io.WriteString(w, `{"error":{"message":"invalid request"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":50}}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(up.Close)
			t.Cleanup(unblock)
			if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "k", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			keys, secrets := newCaller(t, "Capped")
			lim := &access.Limit{Period: "day", Tokens: 1000, Cost: 30}
			setLimit(t, keys[0].ID, lim)
			input, zero := 1000.0, 0.0
			if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			h := New().Front()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); h.ServeHTTP(w, r) }))
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			t.Cleanup(unblock)
			req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+secrets[0])
			client := ts.Client()
			client.Timeout = 10 * time.Second
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if end == "failure" {
				if res.StatusCode != 400 {
					t.Fatalf("failure: %d", res.StatusCode)
				}
			} else {
				if res.StatusCode != 200 {
					t.Fatalf("stream: %d", res.StatusCode)
				}
				if _, err := io.ReadFull(res.Body, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				st := budget.Of(access.Key{ID: keys[0].ID, Limit: lim}, now)
				if st.InFlight != 1 || st.Reserved == 0 || st.ReservedCost == 0 {
					t.Fatalf("stream was not held: %+v", st)
				}
			}
			if end == "disconnect" {
				cancel()
				res.Body.Close()
			} else {
				unblock()
				if _, err := io.Copy(io.Discard, res.Body); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("handler did not release after", end)
			}
			st := budget.Of(access.Key{ID: keys[0].ID, Limit: lim}, now)
			if st.InFlight != 0 || st.Reserved != 0 || st.ReservedCost != 0 {
				t.Fatalf("%s leaked: %+v", end, st)
			}
			if end == "stream" && st.Tokens != 350 {
				t.Fatalf("stream not settled: %+v", st)
			}
			// The client disconnects before the upstream sends its usage frame.
			if end == "disconnect" && (st.Tokens != 0 || st.Cost != 0) {
				t.Fatalf("disconnect settled unreported usage: %+v", st)
			}
		})
	}
}

// The upstream switch still refuses a second request for reservations alone,
// including when the gateway excludes the only candidate of a routing group.
func TestKeyLimitReservationCompatibility(t *testing.T) {
	old := budget.HoldReservations
	budget.HoldReservations = true
	t.Cleanup(func() { budget.HoldReservations = old })
	for _, scoped := range []bool{false, true} {
		t.Run(strconv.FormatBool(scoped), func(t *testing.T) {
			fresh(t)
			budget.Forget()
			t.Cleanup(budget.Forget)
			now := pinLimitClock(t)
			calls := limitedUpstream(t, 0)
			keys, secrets := newCaller(t, "Capped")
			lim := &access.Limit{Period: "day", Tokens: 1000}
			model := "plan/m1"
			if scoped {
				lim.Models = []string{"plan/m1"}
				model = "group/solo"
				if err := provider.SaveGroup(provider.Group{Name: "Solo", Members: []string{"plan/m1"}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
			}
			setLimit(t, keys[0].ID, lim)
			release, refused := budget.Reserve(access.Identity{KeyID: keys[0].ID, KeyName: keys[0].Name, Limit: lim}, 8000, "plan/m1", now)
			if refused != nil {
				t.Fatal(refused)
			}
			t.Cleanup(release)
			h := New().Front()
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			w := keyCall(h, secrets[0], "/v1/chat/completions", body)
			if w.Code != 429 || !strings.Contains(w.Body.String(), "with 1 requests in flight holding the rest") || calls.Load() != 0 {
				t.Fatalf("old refusal: %d %s", w.Code, w.Body.String())
			}
			release()
			if w := keyCall(h, secrets[0], "/v1/chat/completions", body); w.Code != 200 {
				t.Fatalf("released request: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
