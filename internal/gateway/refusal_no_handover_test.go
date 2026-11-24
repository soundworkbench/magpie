package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// This is the Anthropic/Claude subscription refusal shape after the gateway
// has rendered Claude Code's classifier error. safety_filter is not a policy
// code; cyber_policy is what makes policyRefusal recognize the real refusal.
const claudeSafety400 = `{"error":{"type":"invalid_request_error","code":"cyber_policy","message":"refused this request (safety filter: refusal (category cyber: blocked))"}}`

func noHandoverClaude(t *testing.T, status int, body string) *[]string {
	t.Helper()
	var keys []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		keys = append(keys, key)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "a", Name: "A", Models: []string{"m"}, Anthropic: up.URL,
		Routing: provider.Ordered, Key: "k1", Keys: []provider.KeyAccount{{Key: "k2"}}}); err != nil {
		t.Fatal(err)
	}
	return &keys
}

func TestSafetyRefusalDefaultRecordsOneUnhandedRow(t *testing.T) {
	fresh(t)
	keys := noHandoverClaude(t, http.StatusBadRequest, claudeSafety400)
	s := New()
	code, body := sendTo(s, "/v1/messages", messagesAsk)
	if code != http.StatusBadRequest || !strings.Contains(body, "safety filter") || len(*keys) != 1 {
		t.Fatalf("status %d body %q keys %v", code, body, *keys)
	}
	recs := usage.Load(time.Time{})
	if len(recs) != 1 || recs[0].HandedOn || !strings.Contains(recs[0].Error, "safety filter") {
		t.Fatalf("usage rows: %+v", recs)
	}
	if _, ok := restOf("a"); ok {
		t.Fatal("safety refusal rested the account")
	}
}

func TestStopRefusalDefaultDoesNotHandover(t *testing.T) {
	fresh(t)
	keys := noHandoverClaude(t, 0, anthropicRefusalWhole)
	code, body := sendTo(New(), "/v1/messages", strings.Replace(messagesAsk, `,"stream":true`, "", 1))
	if code != http.StatusOK || !strings.Contains(body, `"stop_reason":"refusal"`) || len(*keys) != 1 {
		t.Fatalf("status %d body %q keys %v", code, body, *keys)
	}
}

func TestSafetyRefusalHandoverSwitchOffRestoresOldBehavior(t *testing.T) {
	fresh(t)
	no := false
	s := settings.Load()
	s.NoRefusalHandover = &no
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	keys := keyedClaude(t, "k1")
	code, body := sendTo(New(), "/v1/messages", messagesAsk)
	if code != http.StatusOK || !strings.Contains(body, "from b") || strings.Join(*keys, ",") != "k1,k2" {
		t.Fatalf("status %d body %q keys %v", code, body, *keys)
	}
}

// A routing group keeps a safety refusal on its first member by default. The
// 400 and streamed Anthropic forms both make one upstream call, preserve the
// vendor's refusal in the response and usage row, and leave routing's rest,
// failure and slow counters untouched.
func TestSafetyRefusalGroupDefaultDoesNotHandover(t *testing.T) {
	for _, tc := range []struct {
		name, path, ask string
		reply           reply
		wantCode        int
		wantText        string
	}{
		{"400 safety filter", "/v1/messages", `{"model":"group/g","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, reply{400, "application/json", claudeSafety400}, http.StatusBadRequest, "safety filter"},
		{"stream refusal", "/v1/messages", messagesAsk, reply{200, "text/event-stream", anthropicRefusalDetailed}, http.StatusOK, `"stop_reason":"refusal"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			a := &scripted{replies: []reply{tc.reply}}
			b := &scripted{replies: []reply{{200, "text/event-stream", anthropicAnswer}}}
			scriptedOn(t, "a", provider.Anthropic, a)
			scriptedOn(t, "b", provider.Anthropic, b)
			if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			s := New()
			code, body := sendTo(s, tc.path, strings.Replace(tc.ask, "a/m", "group/g", 1))
			if code != tc.wantCode || !strings.Contains(body, tc.wantText) || a.n != 1 || b.n != 0 {
				t.Fatalf("status %d body %q upstream calls a=%d b=%d", code, body, a.n, b.n)
			}
			recs := usage.Load(time.Time{})
			if len(recs) != 1 || recs[0].HandedOn || !strings.Contains(recs[0].Error, "safety filter") {
				t.Fatalf("usage rows: %+v", recs)
			}
			r := lastRoute(s)
			if len(r.Tries) != 1 || r.Tries[0].Rest != nil || r.Tries[0].Fail != failRefused {
				t.Fatalf("tries: %+v", r.Tries)
			}
			if _, ok := restOf("a"); ok || s.resting("a") || s.resting("a/m") {
				t.Fatal("refusing member was rested")
			}
			routed.Lock()
			failures, slow := len(routed.failures), len(routed.slow)
			routed.Unlock()
			if failures != 0 || slow != 0 {
				t.Fatalf("refusal counted in routing statistics: failures=%d slow=%d", failures, slow)
			}
		})
	}
}

// Turning the switch off restores the legacy group handover for both the
// non-streaming safety error and Anthropic's streamed stop_reason refusal.
func TestSafetyRefusalGroupSwitchOffHandsOver(t *testing.T) {
	for _, tc := range []struct {
		name, path, ask string
		reply           reply
		answer          reply
	}{
		{"400 safety filter", "/v1/messages", `{"model":"group/g","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, reply{400, "application/json", claudeSafety400}, reply{200, "application/json", anthropicAnswerWhole}},
		{"stream refusal", "/v1/messages", messagesAsk, reply{200, "text/event-stream", anthropicRefusalDetailed}, reply{200, "text/event-stream", anthropicAnswer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			off := false
			cfg := settings.Load()
			cfg.NoRefusalHandover = &off
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			a := &scripted{replies: []reply{tc.reply}}
			b := &scripted{replies: []reply{tc.answer}}
			scriptedOn(t, "a", provider.Anthropic, a)
			scriptedOn(t, "b", provider.Anthropic, b)
			if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			code, body := sendTo(New(), tc.path, strings.Replace(tc.ask, "a/m", "group/g", 1))
			if code != http.StatusOK || !strings.Contains(body, "from b") || a.n != 1 || b.n != 1 {
				t.Fatalf("status %d body %q upstream calls a=%d b=%d", code, body, a.n, b.n)
			}
		})
	}
}

// The switch only controls safety refusals. A retryable 529 error after
// thinking is still spliced to the next group member under the default
// configuration. This first request has not established trusted thinking,
// so the held reasoning is dropped when the try is handed over.
func TestRetryableThinkingErrorStillHandsOverByDefault(t *testing.T) {
	fresh(t)
	first := sse(
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"m1","role":"assistant","model":"m","content":[]}}`,
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first thought"}}`,
		`event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
	)
	a := &scripted{replies: []reply{{200, "text/event-stream", first}}}
	b := &scripted{replies: []reply{{200, "text/event-stream", anthropicAnswer}}}
	scriptedOn(t, "a", provider.Anthropic, a)
	scriptedOn(t, "b", provider.Anthropic, b)
	pa, err := provider.Find("a")
	if err != nil {
		t.Fatal(err)
	}
	pa.Models = []string{"claude-opus-5-5"}
	if err := provider.Save(*pa); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/claude-opus-5-5", "b/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	code, body := sendTo(New(), "/v1/messages", claudeCodeAsk)
	if code != http.StatusOK || !strings.Contains(body, "from b") || a.n != 1 || b.n != 1 {
		t.Fatalf("status %d body %q upstream calls a=%d b=%d", code, body, a.n, b.n)
	}
}
