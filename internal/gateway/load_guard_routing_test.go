package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func loadGuardRouting(t *testing.T) map[string]provider.LoadReading {
	t.Helper()
	fresh(t)
	was := remoteLoad
	readings := map[string]provider.LoadReading{}
	remoteLoad = func(p provider.Provider) (provider.LoadReading, bool) { r, ok := readings[p.ID]; return r, ok }
	s := settings.Load()
	old := s.LoadGuard
	s.LoadGuard = settings.LoadGuardConfig{HostFreeMinMiB: 2048, InstanceMaxMiB: 2560, IdleProcessMinutes: 30}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remoteLoad = was; s := settings.Load(); s.LoadGuard = old; settings.Save(s) })
	return readings
}

func loadGuardCandidates() ([]candidate, planned) {
	var cs []candidate
	var pl planned
	for i, id := range []string{"load-first", "load-second", "load-third"} {
		cs = append(cs, candidate{p: provider.Provider{ID: id, Key: "k"}, model: "m", rest: id, place: i + 1})
		pl.order = append(pl.order, Weighed{ID: id, Model: "m"})
	}
	return cs, pl
}

func loadGuardHigh() provider.LoadReading {
	return provider.LoadReading{At: time.Now().Unix(), HostAvailableMiB: 512, HostTotalMiB: 16384, SelfMiB: 100}
}

func TestLoadGuardNewConversation(t *testing.T) {
	readings := loadGuardRouting(t)
	readings["load-first"] = loadGuardHigh()
	cs, pl := loadGuardCandidates()
	cs, pl = restLast(cs, pl)
	if cs[0].p.ID != "load-second" {
		t.Fatalf("new conversation chose %s, want second remote", cs[0].p.ID)
	}
	if pl.order[2].Loaded == "" || pl.order[2].ID != "load-first" {
		t.Fatalf("missing load reason: %+v", pl.order)
	}
	b, err := json.Marshal(pl.order[2])
	if err != nil {
		t.Fatal(err)
	}
	var trace map[string]any
	if err := json.Unmarshal(b, &trace); err != nil {
		t.Fatal(err)
	}
	if trace["loaded"] != pl.order[2].Loaded {
		t.Fatalf("trace lost load reason: %s", b)
	}
}

func TestLoadGuardKeepsAffinity(t *testing.T) {
	readings := loadGuardRouting(t)
	for _, ordered := range []bool{false, true} {
		for _, tc := range []struct{ name, mode, body string }{
			{"session", provider.AffinitySession, `{"messages":[{"role":"user","content":"hello"}]}`},
			{"turn", provider.AffinityTurn, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`},
			{"cache", "", `{"messages":[{"role":"user","content":"hello"}]}`},
		} {
			t.Run(tc.name+itoa(boolInt(ordered)), func(t *testing.T) {
				cs, pl := loadGuardCandidates()
				h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
				body := []byte(tc.body)
				key := stickKey("group/load", h, body)
				answered(key, cs[0], 1, Usage{CacheRead: 5000})
				t.Cleanup(func() { sticks.Lock(); delete(sticks.m, key); sticks.Unlock() })
				readings[cs[0].p.ID] = loadGuardHigh()
				cs, pl = restLast(cs, pl)
				got, _, aff, _ := affine("group/load", tc.mode, false, false, ordered, h, provider.Chat, body, body, cs, pl)
				if got[0].p.ID != "load-first" || !aff.Kept || aff.Why != tc.name {
					t.Fatalf("loaded answerer moved: %s, %+v", got[0].p.ID, aff)
				}
			})
		}
	}
}

func TestLoadGuardRecoveryKeepsConversation(t *testing.T) {
	readings := loadGuardRouting(t)
	for _, route := range []string{"smart", "order", "fallback"} {
		t.Run(route, func(t *testing.T) {
			cs, pl := loadGuardCandidates()
			if route == "fallback" {
				pl.order[1].Fallback = true
				pl.order[2].Fallback = true
			}
			readings["load-first"] = loadGuardHigh()
			cs, pl = restLast(cs, pl)
			if cs[0].p.ID != "load-second" {
				t.Fatalf("loaded first was chosen: %s", cs[0].p.ID)
			}
			stood := standsInFor(cs[0], cs, pl.order)
			if stood {
				t.Fatal("load deferral counted as a stand-in")
			}
			body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
			scope := "group/load"
			if route == "fallback" {
				scope = "load-first"
			}
			key := stickKey(scope, h, body)
			answeredStanding(key, cs[0], 1, Usage{CacheRead: 5000}, stood)
			t.Cleanup(func() { sticks.Lock(); delete(sticks.m, key); sticks.Unlock() })
			delete(readings, "load-first")
			cs, pl = loadGuardCandidates()
			if route == "fallback" {
				pl.order[1].Fallback = true
				pl.order[2].Fallback = true
			}
			cs, pl = restLast(cs, pl)
			got, _, aff, _ := affine(scope, "", false, false, route == "order", h, provider.Chat, body, body, cs, pl)
			if got[0].p.ID != "load-second" || !aff.Kept || aff.Why != "cache" {
				t.Fatalf("load recovery moved conversation: %s, %+v", got[0].p.ID, aff)
			}
		})
	}
}

func TestLoadGuardAllLoadedAndUnknown(t *testing.T) {
	readings := loadGuardRouting(t)
	cs, pl := loadGuardCandidates()
	want := append([]candidate(nil), cs...)
	for _, c := range cs {
		readings[c.p.ID] = loadGuardHigh()
	}
	got, planned := restLast(cs, pl)
	for i, c := range got {
		if c.p.ID != want[i].p.ID || planned.order[i].Loaded == "" {
			t.Fatalf("all loaded order: %+v", planned.order)
		}
	}
	clear(readings)
	cs, pl = loadGuardCandidates()
	got, planned = restLast(cs, pl)
	if !reflect.DeepEqual(got, cs) || !reflect.DeepEqual(planned, pl) {
		t.Fatalf("unknown load changed plan: %+v", planned)
	}
}

func TestLoadGuardDisabledMatchesNoReadings(t *testing.T) {
	readings := loadGuardRouting(t)
	cs, pl := loadGuardCandidates()
	want, wantPlan := restLast(cs, pl)
	for _, c := range cs {
		readings[c.p.ID] = loadGuardHigh()
	}
	s := settings.Load()
	s.LoadGuard.Off = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	cs, pl = loadGuardCandidates()
	got, gotPlan := restLast(cs, pl)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotPlan, wantPlan) {
		t.Fatalf("disabled differs: %+v / %+v", got, gotPlan)
	}
}

func TestLoadGuardBucketsAndReadiness(t *testing.T) {
	readings := loadGuardRouting(t)
	busyLoadAt(t, 80)
	plugin.ForgetLoads()
	t.Cleanup(plugin.ForgetLoads)
	plugin.NoteLoadsForTest("load-plugin", map[string]float64{"m": 95}, time.Now())
	cs, pl := loadGuardCandidates()
	cs[1].p = provider.PluginForTest("load-second", "load-plugin")
	readings["load-first"] = loadGuardHigh()
	resting := candidate{p: provider.Provider{ID: "load-rest"}, model: "m", rest: "load-rest"}
	restingUntil.Lock()
	restingUntil.m[resting.restKey()] = time.Now().Add(time.Minute)
	restingUntil.Unlock()
	t.Cleanup(func() { restingUntil.Lock(); delete(restingUntil.m, resting.restKey()); restingUntil.Unlock() })
	cs = append(cs, resting)
	pl.order = append(pl.order, Weighed{ID: resting.rest, Model: "m"})
	got, plan := restLast(cs, pl)
	want := []string{"load-third", "load-second", "load-first", "load-rest"}
	for i, c := range got {
		if c.p.ID != want[i] {
			t.Fatalf("bucket order at %d: %s, want %s", i, c.p.ID, want[i])
		}
	}
	if readyForNew(plan.order, 2) {
		t.Fatal("loaded remote ready for a new conversation")
	}
}

func TestLoadGuardSpreadAndBurst(t *testing.T) {
	readings := loadGuardRouting(t)
	readings["load-first"] = loadGuardHigh()
	for _, all := range []bool{false, true} {
		cs, pl := loadGuardCandidates()
		if all {
			for _, c := range cs {
				readings[c.p.ID] = loadGuardHigh()
			}
		}
		cs, pl = restLast(cs, pl)
		scope := "group/load-spread"
		key := scope + "|new"
		held := scope + "|held"
		sticks.Lock()
		sticks.m[held] = stick{who: cs[0].who(), model: cs[0].model, at: time.Now()}
		sticks.Unlock()
		got, _ := leastHeld(scope, key, cs, pl, false)
		sticks.Lock()
		delete(sticks.m, held)
		delete(sticks.opening, key)
		sticks.Unlock()
		want := "load-third"
		if all {
			want = "load-first"
		}
		if got[0].p.ID != want {
			t.Fatalf("spread all=%v: %s, want %s", all, got[0].p.ID, want)
		}
		for i, w := range pl.order {
			if w.Loaded != "" && i > 0 && burstAlike(provider.Pace, nil, false, cs, pl.order, i) {
				t.Fatal("burst spread promoted a loaded remote")
			}
		}
	}
}

func TestLoadGuardDisabledAffinity(t *testing.T) {
	readings := loadGuardRouting(t)
	cs, _ := loadGuardCandidates()
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	key := stickKey("group/load-disabled", h, body)
	cs[1].loadChosen = true
	answered(key, cs[1], 1, Usage{CacheRead: 5000})
	t.Cleanup(func() { sticks.Lock(); delete(sticks.m, key); sticks.Unlock() })
	s := settings.Load()
	s.LoadGuard.Off = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	cs, pl := loadGuardCandidates()
	cs, pl = restLast(cs, pl)
	want, wantPlan, wantAff, _ := affine("group/load-disabled", "", false, false, true, h, provider.Chat, body, body, cs, pl)
	for _, c := range cs {
		readings[c.p.ID] = loadGuardHigh()
	}
	cs, pl = loadGuardCandidates()
	cs, pl = restLast(cs, pl)
	got, gotPlan, gotAff, _ := affine("group/load-disabled", "", false, false, true, h, provider.Chat, body, body, cs, pl)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotPlan, wantPlan) || !reflect.DeepEqual(gotAff, wantAff) || gotAff.Why != "order" {
		t.Fatalf("off changed affinity: %+v, want %+v", gotAff, wantAff)
	}
	for i := range pl.order {
		if !readyForNew(pl.order, i) {
			t.Fatal("off changed readiness")
		}
	}
}

func TestLoadGuardRecoverySurvivesRestart(t *testing.T) {
	readings := loadGuardRouting(t)
	readings["load-first"] = loadGuardHigh()
	cs, pl := loadGuardCandidates()
	cs, pl = restLast(cs, pl)
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	key := stickKey("group/load-restart", h, body)
	answeredStanding(key, cs[0], 1, Usage{CacheRead: 5000}, standsInFor(cs[0], cs, pl.order))
	flushSticks()
	sticks.Lock()
	sticks.m = map[string]stick{}
	sticks.from = ""
	sticks.mod = time.Time{}
	sticks.Unlock()
	clear(readings)
	cs, pl = loadGuardCandidates()
	cs, pl = restLast(cs, pl)
	got, _, aff, _ := affine("group/load-restart", "", false, false, true, h, provider.Chat, body, body, cs, pl)
	if got[0].p.ID != "load-second" || !aff.Kept || aff.Why != "cache" {
		t.Fatalf("restart forgot load deferral: %s, %+v", got[0].p.ID, aff)
	}
}

func TestLoadGuardRoutesRequests(t *testing.T) {
	readings := loadGuardRouting(t)
	// The fake readings are injected at the routing boundary; every upstream
	// here serves only chat, so no status request can affect the assertion.
	for _, id := range []string{"load-first", "load-second", "load-third"} {
		serveOn(t, id, id, []string{"m"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":3000,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2500}}}`, key)
		}))
	}
	if err := provider.SaveGroup(provider.Group{ID: "load-route", Name: "Load route", Routing: provider.Ordered, Members: []string{"load-first/m", "load-second/m", "load-third/m"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	body := `{"model":"group/load-route","messages":[{"role":"user","content":"hello"}]}`
	ask := func(session, want string) {
		t.Helper()
		code, reply := postAs(t, s, session, body)
		if code != 200 || !strings.Contains(reply, want) {
			t.Fatalf("%s: %d %s, want %s", session, code, reply, want)
		}
	}
	readings["load-first"] = loadGuardHigh()
	ask("load-kept", "load-second")
	clear(readings)
	ask("load-kept", "load-second")
	ask("load-new", "load-first")
	for _, id := range []string{"load-first", "load-second", "load-third"} {
		readings[id] = loadGuardHigh()
	}
	ask("load-all", "load-first")
}
