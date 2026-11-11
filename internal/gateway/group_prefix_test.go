package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// Vendor prefixes sent by clients need not be channels on this gateway.
// They may name a group, but cannot take a channel, catalog id or key's
// existing access away from it.
func TestPrefixedModelGroup(t *testing.T) {
	const opus = "claude-opus-5-5"
	for _, tc := range []struct {
		name, asked, channel, alias, mapped, model, path string
		off, group                                       bool
		models                                           []string
		status                                           int
		served                                           string
	}{
		{name: "unknown prefix", asked: "vendor/" + opus, group: true},
		{name: "Claude prefix", asked: "claude/" + opus, group: true},
		{name: "Claude Code Messages", asked: "claude/" + opus, path: "/v1/messages", group: true},
		{name: "Sonnet prefix", asked: "claude/claude-sonnet-5-5", model: "claude-sonnet-5-5", group: true},
		{name: "Haiku prefix", asked: "claude/claude-haiku-5-5", model: "claude-haiku-5-5", group: true},
		{name: "older Opus prefix", asked: "claude/claude-opus-4-8", model: "claude-opus-4-8", group: true},
		{name: "explicit group", asked: "group/" + opus, group: true},
		{name: "bare name", asked: opus, group: true},
		{name: "disabled channel", asked: "vendor/" + opus, channel: "vendor", off: true, status: http.StatusNotFound},
		{name: "disabled alias", asked: "old/" + opus, channel: "renamed", alias: "old", off: true, status: http.StatusNotFound},
		{name: "enabled channel", asked: "vendor/" + opus, channel: "vendor", served: "vendor"},
		{name: "enabled alias", asked: "old/" + opus, channel: "renamed", alias: "old", served: "renamed"},
		{name: "channel name", asked: "VENDOR/" + opus, channel: "vendor", served: "vendor"},
		{name: "explicit remote", asked: "remote-a/claude/" + opus, served: "remote-a"},
		{name: "exact catalog ID", asked: "remote-a/claude/" + opus, mapped: "claude/" + opus, served: "remote-a"},
		{name: "Codex alias", asked: "codex/gpt-6.1-sol", channel: "remote-codex", alias: "codex", served: "remote-codex"},
		{name: "Codex model mapping", asked: "codex/gpt-6.1-sol", channel: "remote-codex", alias: "codex", mapped: "gpt-6.1-sol", served: "remote-codex"},
		{name: "DeepSeek channel", asked: "deepseek/deepseek-flash", channel: "deepseek", served: "deepseek"},
		{name: "no matching group", asked: "claude/no-group", served: "remote-a"},
		{name: "unknown model", asked: "vendor/no-group", status: 404},
		{name: "group and members allowed", asked: "claude/" + opus, models: []string{"claude/*", "group/claude-*", "remote-*/claude/*"}, group: true},
		{name: "group alone allowed", asked: "claude/" + opus, models: []string{"group/claude-*"}, group: true},
		{name: "members allowed without group", asked: "claude/" + opus, models: []string{"claude/*", "remote-*/claude/*"}, served: "remote-a"},
		{name: "wrong group stays refused", asked: "claude/" + opus, models: []string{"group/unrelated"}, status: 403},
		{name: "group denied keeps unknown model", asked: "vendor/" + opus, models: []string{"claude/*"}, status: 404},
		{name: "Claude only stays refused", asked: "claude/" + opus, models: []string{"claude/*"}, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			if err := provider.SetAutoGroups(false); err != nil {
				t.Fatal(err)
			}
			model := tc.model
			if model == "" {
				model = opus
			}
			if strings.Contains(tc.asked, "gpt-") {
				model = "gpt-6.1-sol"
			} else if strings.Contains(tc.asked, "deepseek-") {
				model = "deepseek-flash"
			}
			save := func(p provider.Provider) {
				t.Helper()
				p.Name, p.Key, p.Chat = strings.ToUpper(p.ID), p.ID, "http://127.0.0.1:1/"+p.ID+"/v1"
				if err := provider.Save(p); err != nil {
					t.Fatal(err)
				}
				if p.IsRemoteMagpie() {
					var ms []catalog.Model
					for _, m := range p.Models {
						ms = append(ms, catalog.Model{ID: m, APIs: []string{"chat", "anthropic"}})
					}
					if err := catalog.SaveLive(p.ID, p.Chat, ms); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, id := range []string{"remote-a", "remote-b"} {
				save(provider.Provider{ID: id, Preset: provider.RemoteMagpiePreset, Models: []string{"claude/" + model, "claude/no-group"}})
			}
			if tc.channel != "" {
				p := provider.Provider{ID: tc.channel, Off: tc.off, Models: []string{model, "wire-model"}}
				if tc.alias != "" {
					p.Was = []string{tc.alias}
				}
				save(p)
			}
			if tc.mapped != "" {
				ref := tc.channel + "/wire-model"
				if tc.channel == "" {
					ref = "remote-a/claude/no-group"
				}
				if _, _, err := provider.SetModelID(ref, tc.mapped); err != nil {
					t.Fatal(err)
				}
			}
			if err := provider.SaveGroup(provider.Group{ID: model, Name: model, Members: []string{"remote-b/claude/" + model, "remote-a/claude/" + model}, Routing: provider.Pace}); err != nil {
				t.Fatal(err)
			}
			secret := ""
			if tc.models != nil {
				keys, secrets := newCaller(t, "Claude")
				secret = secrets[0]
				if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: tc.models}); err != nil {
					t.Fatal(err)
				}
			}
			s := New()
			var requests atomic.Int32
			s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				defer r.Body.Close()
				id := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
				w := httptest.NewRecorder()
				prefixedModelReply(id).ServeHTTP(w, r)
				return w.Result(), nil
			})}
			path := tc.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"`+tc.asked+`","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
			if tc.path == "/v1/messages" {
				req.Header.Set("User-Agent", "claude-code/2.1.0")
			}
			if secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			status := tc.status
			if status == 0 {
				status = 200
			}
			if w.Code != status {
				t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
			}
			if tc.off {
				if !strings.Contains(w.Body.String(), "switched off in Magpie") {
					t.Fatalf("missing switched-off refusal: %s", w.Body)
				}
				if got := requests.Load(); got != 0 {
					t.Fatalf("sent %d upstream requests for a switched-off provider, want none", got)
				}
			}
			if tc.models != nil && status != 404 {
				count := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"`+tc.asked+`","messages":[{"role":"user","content":"hi"}]}`))
				count.Header.Set("Authorization", "Bearer "+secret)
				cw := httptest.NewRecorder()
				s.Handler().ServeHTTP(cw, count)
				if cw.Code != status {
					t.Fatalf("count_tokens status %d, want %d: %s", cw.Code, status, cw.Body)
				}
			}
			if status != 200 {
				return
			}
			r := lastRoute(s)
			if tc.group {
				if r.Group == nil || r.Group.ID != model || r.Group.Routing != provider.Pace {
					t.Fatalf("route group = %+v, want %s with pace", r.Group, model)
				}
			} else if r.Group != nil {
				t.Fatalf("unexpected group: %+v", r.Group)
			}
			u := lastUsage(t)
			if tc.group {
				if u.Provider != "remote-a" && u.Provider != "remote-b" {
					t.Fatalf("usage provider = %q, want an actual member", u.Provider)
				}
			} else if u.Provider != tc.served {
				t.Fatalf("usage provider = %q, want %q", u.Provider, tc.served)
			}
			if tc.mapped != "" {
				wire := "wire-model"
				if tc.channel == "" {
					wire = "claude/no-group"
				}
				if !strings.Contains(w.Body.String(), "model "+wire) {
					t.Fatalf("mapped model did not reach its wire id %q: %s", wire, w.Body)
				}
			}
			if !strings.Contains(w.Body.String(), "from "+u.Provider) {
				t.Fatalf("usage provider %q differs from reply %s", u.Provider, w.Body)
			}
		})
	}
}

// The remote gateway replies in the client's native protocol and echoes
// the model it received, so a catalog mapping is checked on the wire too.
func prefixedModelReply(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		text := "from " + id + " model " + body.Model
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			json.NewEncoder(w).Encode(map[string]int{"input_tokens": 5})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant", "model": body.Model,
				"content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn",
				"usage": map[string]int{"input_tokens": 3000, "output_tokens": 5},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chat_1", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 3000, "completion_tokens": 5},
		})
	})
}
