package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// Counts use the same group and permissions as Messages. The group's
// first member is the second provider, so direct resolution cannot pass.
func TestPrefixedModelGroupCounts(t *testing.T) {
	const model = "claude-haiku-5-5"
	for _, tc := range []struct {
		name, asked, channel, alias, served string
		models                              []string
		off, accounts, disabled, group      bool
		status                              int
	}{
		{name: "Claude prefix", asked: "claude/" + model, served: "remote-b", group: true},
		{name: "unknown prefix", asked: "vendor/" + model, served: "remote-b", group: true},
		{name: "group alone allowed", asked: "claude/" + model, models: []string{"group/" + model}, served: "remote-b", group: true},
		{name: "group denied", asked: "claude/" + model, models: []string{"group/unrelated"}, status: 403},
		{name: "members allowed without group", asked: "claude/" + model, models: []string{"remote-*/claude/*"}, served: "remote-a"},
		{name: "enabled provider", asked: "vendor/" + model, channel: "vendor", served: "vendor"},
		{name: "enabled alias", asked: "old/" + model, channel: "vendor", alias: "old", served: "vendor"},
		{name: "exact remote ID", asked: "remote-a/claude/" + model, served: "remote-a"},
		{name: "disabled provider", asked: "vendor/" + model, channel: "vendor", off: true, status: 404},
		{name: "disabled alias", asked: "old/" + model, channel: "vendor", alias: "old", off: true, status: 404},
		{name: "self prefix", asked: model + "/" + model, served: "remote-b", group: true},
		{name: "self prefix group allowed", asked: model + "/" + model, models: []string{"group/" + model}, served: "remote-b", group: true},
		{name: "self prefix enabled provider", asked: model + "/" + model, channel: model, served: model},
		{name: "self prefix disabled provider", asked: model + "/" + model, channel: model, off: true, status: 404},
		{name: "group accounts", asked: "claude/" + model, accounts: true, served: "remote-b", group: true},
		{name: "disabled group members", asked: "claude/" + model, disabled: true, status: 403},
		{name: "bare group", asked: model, served: "remote-b", group: true},
		{name: "explicit group", asked: "group/" + model, served: "remote-b", group: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			if err := provider.SetAutoGroups(false); err != nil {
				t.Fatal(err)
			}
			save := func(id string, remote bool) {
				t.Helper()
				base := "http://127.0.0.1:1/" + id + "/v1"
				p := provider.Provider{ID: id, Name: id, Key: id, Chat: base, Anthropic: base, Models: []string{model}}
				if remote {
					p.Preset = provider.RemoteMagpiePreset
					p.Models = []string{"claude/" + model}
				} else {
					p.Off = tc.off
					if tc.alias != "" {
						p.Was = []string{tc.alias}
					}
				}
				if tc.accounts && id == "remote-b" {
					p.Keys = []provider.KeyAccount{{Key: "group-key"}}
				}
				if err := provider.Save(p); err != nil {
					t.Fatal(err)
				}
				if remote {
					if err := catalog.SaveLive(id, base, []catalog.Model{{ID: "claude/" + model, APIs: []string{"chat", "anthropic"}}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			save("remote-a", true)
			save("remote-b", true)
			if tc.channel != "" {
				save(tc.channel, false)
			}
			g := provider.Group{ID: model, Name: model, Members: []string{"remote-b/claude/" + model, "remote-a/claude/" + model}, Routing: provider.Ordered}
			if tc.accounts {
				g.Accounts = []string{"remote-b/" + provider.KeyID("group-key")}
			}
			if err := provider.SaveGroup(g); err != nil {
				t.Fatal(err)
			}
			if tc.disabled {
				if err := provider.DisableModels("remote-*/claude/" + model); err != nil {
					t.Fatal(err)
				}
			}
			secret := ""
			if tc.models != nil {
				keys, secrets := newCaller(t, "Group caller")
				secret = secrets[0]
				if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: tc.models}); err != nil {
					t.Fatal(err)
				}
			}
			type upstreamCall struct{ provider, model, key string }
			var mu sync.Mutex
			var calls []upstreamCall
			s := New()
			s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				var req struct{ Model string }
				if err := json.Unmarshal(body, &req); err != nil {
					return nil, err
				}
				id := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
				mu.Lock()
				calls = append(calls, upstreamCall{id, req.Model, r.Header.Get("X-Api-Key")})
				mu.Unlock()
				w := httptest.NewRecorder()
				if strings.HasSuffix(r.URL.Path, "/count_tokens") {
					w.Header().Set("Content-Type", "application/json")
					tokens := 101
					if id == "remote-b" {
						tokens = 202
					}
					json.NewEncoder(w).Encode(map[string]int{"input_tokens": tokens})
				} else {
					r.Body = io.NopCloser(bytes.NewReader(body))
					prefixedModelReply(id).ServeHTTP(w, r)
				}
				return w.Result(), nil
			})}
			h := s.Handler()
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1beta/models/" + tc.asked + ":countTokens"} {
				t.Run(path, func(t *testing.T) {
					mu.Lock()
					calls = nil
					mu.Unlock()
					body := `{"model":"` + tc.asked + `","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
					gemini := strings.HasPrefix(path, "/v1beta/")
					if gemini {
						body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
					}
					w := askGroup(h, secret, path, body)
					status := tc.status
					if status == 0 {
						status = http.StatusOK
					}
					if w.Code != status {
						t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
					}
					mu.Lock()
					got := append([]upstreamCall(nil), calls...)
					mu.Unlock()
					if tc.off && !strings.Contains(w.Body.String(), "switched off in Magpie") {
						t.Fatalf("missing provider refusal: %s", w.Body)
					}
					if tc.disabled && !strings.Contains(w.Body.String(), "every model of the routing group") {
						t.Fatalf("missing group refusal: %s", w.Body)
					}
					if status != http.StatusOK || gemini {
						if len(got) != 0 {
							t.Fatalf("unexpected upstream calls: %+v", got)
						}
						return
					}
					wire, key := model, tc.served
					if strings.HasPrefix(tc.served, "remote-") {
						wire = "claude/" + model
					}
					if tc.accounts {
						key = "group-key"
					}
					if len(got) != 1 || got[0] != (upstreamCall{tc.served, wire, key}) {
						t.Fatalf("upstream calls = %+v, want %s model %s key %s", got, tc.served, wire, key)
					}
					if path == "/v1/messages" {
						route := lastRoute(s)
						if tc.group && (route.Group == nil || route.Group.ID != model) || !tc.group && route.Group != nil {
							t.Fatalf("route group = %+v, want group %t", route.Group, tc.group)
						}
					} else {
						var count struct {
							Tokens int `json:"input_tokens"`
						}
						want := 101
						if tc.served == "remote-b" {
							want = 202
						}
						if json.Unmarshal(w.Body.Bytes(), &count) != nil || count.Tokens != want {
							t.Fatalf("count = %s, want upstream count %d", w.Body, want)
						}
					}
				})
			}
		})
	}
}

// A prefix kept to its agent or left out as this magpie must not turn
// a refused Messages request into a successful local token estimate.
func TestPrefixedModelGroupCountsRefusedPrefixes(t *testing.T) {
	const opus = "claude-opus-5-5"
	for _, tc := range []struct {
		name, asked, refusal string
		kept                 bool
	}{
		{name: "kept subscription", asked: "Claude/" + opus, kept: true, refusal: "kept to Claude Code's own sign-in"},
		// ThroughSelf leaves the bare model, but the other remotes only list
		// "claude/" + opus, so this name cannot resolve through them.
		{name: "self remote", asked: "remote-self/" + opus, refusal: "a Remote magpie that is this magpie itself"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			writeJSON := func(path string, v any) {
				t.Helper()
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kept {
				t.Cleanup(provider.ForgetAccounts)
				// Account discovery relies on the claude stand-in installed by TestMain.
				config := t.TempDir()
				t.Setenv("CLAUDE_CONFIG_DIR", config)
				writeJSON(filepath.Join(config, ".credentials.json"), map[string]any{
					"claudeAiOauth": map[string]any{
						"accessToken": "test-access-token", "refreshToken": "test-refresh-token",
						"expiresAt": time.Now().Add(time.Hour).UnixMilli(), "subscriptionType": "max",
						"scopes": []string{"user:inference", "user:profile"},
					},
				})
				provider.ForgetAccounts()
				if err := provider.SetSwitchOnly("claude", true); err != nil {
					t.Fatal(err)
				}
				p, err := provider.Find("claude")
				if err != nil || !p.KeptToAgent() || p.On() || p.Off {
					t.Fatal("expected a subscription kept to its agent without being switched off")
				}
			} else {
				was := provider.LeaveOutSelf
				provider.LeaveOutSelf = true
				t.Cleanup(func() { provider.LeaveOutSelf = was })
				// Seed discovery before the provider catalog reads it.
				writeJSON(filepath.Join(filepath.Dir(provider.Path()), "remote-self.json"), map[string]string{
					"remote-self": "http://127.0.0.1:1/remote-self/v1",
				})
			}
			if err := provider.SetAutoGroups(false); err != nil {
				t.Fatal(err)
			}
			ids := []string{"remote-a", "remote-b"}
			if !tc.kept {
				ids = append(ids, "remote-self")
			}
			for _, id := range ids {
				model := "claude/" + opus
				if id == "remote-self" {
					model = opus
				}
				base := "http://127.0.0.1:1/" + id + "/v1"
				if err := provider.Save(provider.Provider{ID: id, Name: id, Key: id, Chat: base, Anthropic: base, Preset: provider.RemoteMagpiePreset, Models: []string{model}}); err != nil {
					t.Fatal(err)
				}
				if err := catalog.SaveLive(id, base, []catalog.Model{{ID: model, APIs: []string{"chat", "anthropic"}}}); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.kept {
				p, err := provider.Find("remote-self")
				if err != nil || !p.SelfLeftOut() || p.On() || p.Off {
					t.Fatal("expected a remote left out as this magpie without being switched off")
				}
			}
			if err := provider.SaveGroup(provider.Group{ID: opus, Name: opus, Members: []string{"remote-b/claude/" + opus, "remote-a/claude/" + opus}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			s := New()
			var requests atomic.Int32
			s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				defer r.Body.Close()
				w := httptest.NewRecorder()
				w.WriteHeader(http.StatusInternalServerError)
				return w.Result(), nil
			})}
			h := s.Handler()
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1beta/models/" + tc.asked + ":countTokens"} {
				t.Run(path, func(t *testing.T) {
					requests.Store(0)
					body := `{"model":"` + tc.asked + `","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
					if strings.HasPrefix(path, "/v1beta/") {
						body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
					}
					w := askGroup(h, "", path, body)
					if w.Code != http.StatusNotFound {
						t.Errorf("status %d, want %d: %s", w.Code, http.StatusNotFound, w.Body)
					}
					if !strings.Contains(w.Body.String(), tc.refusal) {
						t.Errorf("missing refusal %q: %s", tc.refusal, w.Body)
					}
					if got := requests.Load(); got != 0 {
						t.Errorf("sent %d upstream requests for a refused prefix, want none", got)
					}
				})
			}
		})
	}
}
