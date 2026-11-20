package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestClaudeCacheTTLPolicy(t *testing.T) {
	fresh(t)
	for _, tc := range []struct {
		name                               string
		config                             settings.ClaudeCacheTTLConfig
		model, id, keyName, incoming, want string
	}{
		{name: "all", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, want: "1h"},
		{name: "off"},
		{name: "unknown ttl", config: settings.ClaudeCacheTTLConfig{TTL: "5m"}},
		{name: "group glob", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"claude-opus-*"}}, model: "claude-opus-5-5", want: "1h"},
		{name: "other model", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"claude-opus-*"}}, model: "claude-sonnet-5"},
		{name: "star spans slash", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"*"}}, model: "group/work", want: "1h"},
		{name: "key id", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"id"}}, id: "id", want: "1h"},
		{name: "key name", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"Team"}}, id: "id", keyName: "Team", want: "1h"},
		{name: "key name exact", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"team"}}, id: "id", keyName: "Team"},
		{name: "both", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"id"}, Groups: []string{"group/*"}}, model: "group/work", id: "id", want: "1h"},
		{name: "both miss key", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"id"}, Groups: []string{"group/*"}}, model: "group/work", id: "other"},
		{name: "both miss group", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"id"}, Groups: []string{"group/*"}}, model: "claude-opus-5-5", id: "id"},
		{name: "except id", config: settings.ClaudeCacheTTLConfig{TTL: "1h", ExceptKeys: []string{"id"}}, id: "id"},
		{name: "except name", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Keys: []string{"id"}, ExceptKeys: []string{"Team"}}, id: "id", keyName: "Team"},
		{name: "explicit 5m", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "5m", want: "5m"},
		{name: "explicit 1h", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "1h", want: "1h"},
		{name: "default", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "default", want: "1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := settings.Load()
			cfg.ClaudeCacheTTL = tc.config
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			s := New()
			ctx := access.WithIdentity(context.Background(), access.Identity{KeyID: tc.id, KeyName: tc.keyName})
			ctx = s.withClaudeCacheTTL(ctx, tc.model)
			ctx = context.WithValue(ctx, callerCtx{}, caller{cacheTTL: tc.incoming})
			for range 2 {
				req := httptest.NewRequest("POST", "/v1/messages", nil)
				passOnCaller(ctx, req)
				if got := req.Header.Get(ClaudeCacheTTLHeader); got != tc.want {
					t.Fatalf("header %q, want %q", got, tc.want)
				}
			}
			wantCount := int64(0)
			if tc.want == "1h" && tc.incoming != "1h" {
				wantCount = 1
			}
			if got := s.claudeCacheForced.Load(); got != wantCount {
				t.Fatalf("retries counted %d, want %d", got, wantCount)
			}
		})
	}
}

// Two real handlers exercise parsing, routing to a remote member and
// stripping magpie's header before that member reaches a direct vendor.
func TestClaudeCacheTTLForwarding(t *testing.T) {
	fresh(t)
	var mu sync.Mutex
	var told, leaked []string
	v := &cacheWriter{wrote: 3000}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if ttl := r.Header.Get(ClaudeCacheTTLHeader); ttl != "" {
			leaked = append(leaked, ttl)
		}
		mu.Unlock()
		v.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m"}, Anthropic: up.URL}); err != nil {
		t.Fatal(err)
	}
	there := New().Handler()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mu.Lock()
			told = append(told, r.Header.Get(ClaudeCacheTTLHeader))
			mu.Unlock()
		}
		there.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	id, err := provider.Add(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Chat: strings.TrimPrefix(remote.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	office, err := provider.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := office.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-opus-test", "claude-sonnet-test"} {
		if err := provider.SaveGroup(provider.Group{ID: id, Name: id, Members: []string{"office/fake/m"}}); err != nil {
			t.Fatal(err)
		}
	}
	s := New()
	for _, tc := range []struct {
		name                  string
		config                settings.ClaudeCacheTTLConfig
		model, incoming, want string
		magpie, direct        bool
	}{
		{name: "all", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, want: "1h"},
		{name: "hot disable"},
		{name: "except id", config: settings.ClaudeCacheTTLConfig{TTL: "1h", ExceptKeys: []string{"entry-key"}}},
		{name: "except name", config: settings.ClaudeCacheTTLConfig{TTL: "1h", ExceptKeys: []string{"Team"}}},
		{name: "group before rewrite", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"group/claude-opus-*"}}, want: "1h"},
		{name: "sonnet", config: settings.ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"group/claude-opus-*"}}, model: "group/claude-sonnet-test"},
		{name: "magpie 5m", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "5m", want: "5m", magpie: true},
		{name: "magpie default", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "default", want: "1h", magpie: true},
		{name: "direct vendor", config: settings.ClaudeCacheTTLConfig{TTL: "1h"}, incoming: "5m", direct: true, magpie: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := settings.Load()
			cfg.ClaudeCacheTTL = tc.config
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			model := tc.model
			if model == "" {
				model = "group/claude-opus-test"
			}
			if tc.direct {
				model = "fake/m"
			}
			body := `{"model":"` + model + `","max_tokens":16,"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("User-Agent", "external-client/1")
			if tc.magpie {
				req.Header.Set("User-Agent", magpieUA())
			}
			req.Header.Set(ClaudeCacheTTLHeader, tc.incoming)
			req = req.WithContext(access.WithIdentity(req.Context(), access.Identity{KeyID: "entry-key", KeyName: "Team"}))
			mu.Lock()
			before := len(told)
			mu.Unlock()
			count := s.claudeCacheForced.Load()
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from k") {
				t.Fatalf("reply %d %s", rec.Code, rec.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.direct {
				if len(told) != before {
					t.Fatal("direct request reached remote")
				}
			} else if len(told) != before+1 || told[before] != tc.want {
				t.Fatalf("remote headers %v, want one more %q", told, tc.want)
			}
			if len(leaked) != 0 {
				t.Fatalf("vendor received cache TTL header: %v", leaked)
			}
			wantCount := count
			if tc.want == "1h" {
				wantCount++
			}
			if got := s.claudeCacheForced.Load(); got != wantCount {
				t.Fatalf("forced count %d, want %d", got, wantCount)
			}
		})
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/magpie/concurrency", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	s.concurrency(rec, req)
	var stats struct {
		ClaudeCacheTTL struct {
			Forced int64 `json:"forced"`
		} `json:"claudeCacheTTL"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil || rec.Code != 200 || stats.ClaudeCacheTTL.Forced != s.claudeCacheForced.Load() {
		t.Fatalf("concurrency: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}
