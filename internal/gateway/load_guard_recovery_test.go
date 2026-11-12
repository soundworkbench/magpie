package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

func TestLoadGuardSpentAnswererStays(t *testing.T) {
	for _, mode := range []string{"", provider.AffinitySession, provider.AffinityTurn} {
		t.Run(mode, func(t *testing.T) {
			readings := loadGuardRouting(t)
			cs, pl := loadGuardCandidates()
			pl.order[0].Known, pl.order[0].Used = true, 99
			body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			why := "cache"
			if mode == provider.AffinitySession {
				why = "session"
			}
			if mode == provider.AffinityTurn {
				why = "turn"
				body = []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"ok"}]}`)
			}
			h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
			answered(stickKey("group/load", h, body), cs[0], 1, Usage{CacheRead: 5000})
			readings[cs[0].p.ID] = loadGuardHigh()
			cs, pl = restLast(cs, pl)
			got, _, aff, _ := affine("group/load", mode, false, false, true, h, provider.Chat, body, body, cs, pl)
			if got[0].p.ID != "load-first" || !aff.Kept || aff.Why != why {
				t.Fatalf("loaded nearly spent answerer moved: %s, %+v", got[0].p.ID, aff)
			}
		})
	}
}

func TestLoadGuardSpentAnswererOriginallyBehind(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		t.Run(itoa(boolInt(loaded)), func(t *testing.T) {
			readings := loadGuardRouting(t)
			cs, pl := loadGuardCandidates()
			// Smart already preferred the first candidate before load was read.
			pl.order[1].Known, pl.order[1].Used = true, 99
			body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
			answered(stickKey("group/load", h, body), cs[1], 1, Usage{CacheRead: 5000})
			if loaded {
				readings[cs[1].p.ID] = loadGuardHigh()
			}
			cs, pl = restLast(cs, pl)
			got, _, aff, _ := affine("group/load", "", false, false, false, h, provider.Chat, body, body, cs, pl)
			if got[0].p.ID != "load-first" || aff.Kept || aff.Why != "spent" {
				t.Fatalf("nearly spent answerer kept despite original order: %s, %+v", got[0].p.ID, aff)
			}
		})
	}
}

func TestLoadGuardFailureRecovery(t *testing.T) {
	for _, route := range []string{"fallback", "order", "stand-in"} {
		for _, stillLoaded := range []bool{false, true} {
			t.Run(route+itoa(boolInt(stillLoaded)), func(t *testing.T) {
				readings := loadGuardRouting(t)
				cs, pl := loadGuardCandidates()
				scope := "group/load"
				if route == "fallback" {
					scope = "load-first"
					pl.order[1].Fallback, pl.order[2].Fallback = true, true
				}
				if route == "fallback" {
					for i := range cs {
						cs[i].place = 0
					}
				}
				main := cs[0]
				readings[main.p.ID] = loadGuardHigh()
				loadGuardRest(main)
				cs, pl = restLast(cs, pl)
				body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
				h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
				cs, pl, _, key := affine(scope, "", false, false, route == "order", h, provider.Chat, body, body, cs, pl)
				answeredStanding(key, cs[0], 1, Usage{CacheRead: 5000}, standsInFor(cs[0], cs, pl.order))
				sticks.Lock()
				marked := sticks.m[key].loadChosen
				sticks.Unlock()
				if marked {
					t.Error("429 fallback was marked as a load seat")
				}
				restingUntil.Lock()
				delete(restingUntil.m, main.restKey())
				restingUntil.Unlock()
				clear(readings)
				if stillLoaded {
					readings["load-second"] = loadGuardHigh()
				}
				cs, pl = loadGuardCandidates()
				if route == "fallback" {
					pl.order[1].Fallback, pl.order[2].Fallback = true, true
				}
				cs, pl = restLast(cs, pl)
				got, _, aff, _ := affine(scope, "", false, false, route == "order", h, provider.Chat, body, body, cs, pl)
				want := map[string]string{"fallback": "main", "order": "order", "stand-in": "back"}[route]
				if got[0].p.ID != main.p.ID || aff.Why != want {
					t.Fatalf("failure recovery blocked: %s, %+v; want %s", got[0].p.ID, aff, want)
				}
			})
		}
	}
}

func TestLoadGuardFailureBeforeLoadedFallbackReturnsMain(t *testing.T) {
	readings := loadGuardRouting(t)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	plan := func() ([]candidate, planned) {
		cs, pl := loadGuardCandidates()
		for i := range cs {
			cs[i].place = 0 // provider fallbacks have no group place
		}
		pl.order[1].Fallback, pl.order[2].Fallback = true, true
		return restLast(cs, pl)
	}
	main, _ := loadGuardCandidates()
	loadGuardRest(main[0])
	readings["load-second"] = loadGuardHigh()
	cs, pl := plan()
	cs, pl, aff, key := affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	if cs[0].p.ID != "load-third" || aff.Why != "first" {
		t.Fatalf("fallback was not seated: %s %+v", cs[0].p.ID, aff)
	}
	answeredStanding(key, cs[0], 1, Usage{CacheRead: 5000}, standsInFor(cs[0], cs, pl.order))
	// No request observes the main still resting after the first answer.
	restingUntil.Lock()
	delete(restingUntil.m, main[0].restKey())
	restingUntil.Unlock()
	clear(readings)
	cs, pl = plan()
	cs, _, aff, _ = affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	if cs[0].p.ID != "load-first" || aff.Why != "main" {
		t.Fatalf("loaded fallback hid main recovery: %s %+v", cs[0].p.ID, aff)
	}
}

func TestLoadGuardSeatSurvivesTemporaryBusy(t *testing.T) {
	readings := loadGuardRouting(t)
	busyLoadAt(t, 80)
	plugin.ForgetLoads()
	t.Cleanup(plugin.ForgetLoads)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	readings["load-first"] = loadGuardHigh()
	for turn := 1; turn <= 3; turn++ {
		switch turn {
		case 2:
			plugin.NoteLoadsForTest("load-plugin", map[string]float64{"m": 95}, time.Now())
		case 3:
			plugin.NoteLoadsForTest("load-plugin", map[string]float64{"m": 0}, time.Now())
			clear(readings)
		}
		cs, pl := loadGuardCandidates()
		cs[0].p = provider.PluginForTest("load-first", "load-plugin")
		cs, pl = restLast(cs, pl)
		if turn == 2 && (cs[2].p.ID != "load-first" || pl.order[2].Busy != 95) {
			t.Fatalf("first member was not busy: %+v", pl.order)
		}
		cs, pl, aff, key := affine("group/load", "", false, false, true, h, provider.Chat, body, body, cs, pl)
		if cs[0].p.ID != "load-second" || turn > 1 && (!aff.Kept || aff.Why != "cache") {
			t.Fatalf("turn %d: temporary busy lost load seat: %s %+v", turn, cs[0].p.ID, aff)
		}
		answeredStanding(key, cs[0], turn, Usage{CacheRead: 5000}, standsInFor(cs[0], cs, pl.order))
	}
}

func TestLoadGuardKeptSeatNotMarked(t *testing.T) {
	readings := loadGuardRouting(t)
	cs, pl := loadGuardCandidates()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	key := stickKey("group/load", h, body)
	answered(key, cs[1], 1, Usage{CacheRead: 5000})
	readings[cs[0].p.ID] = loadGuardHigh()
	cs, pl = restLast(cs, pl)
	cs, pl, aff, _ := affine("group/load", "", false, false, true, h, provider.Chat, body, body, cs, pl)
	if !aff.Kept {
		t.Fatalf("fixture did not keep seat: %+v", aff)
	}
	answeredStanding(key, cs[0], 2, Usage{CacheRead: 5000}, standsInFor(cs[0], cs, pl.order))
	sticks.Lock()
	marked := sticks.m[key].loadChosen
	sticks.Unlock()
	if marked {
		t.Fatal("existing kept conversation acquired loadChosen")
	}
}

func TestLoadGuardOnlyReadyDefers(t *testing.T) {
	for _, kind := range []string{"ready", "rest", "busy", "aside", "reserve", "credits"} {
		t.Run(kind, func(t *testing.T) {
			readings := loadGuardRouting(t)
			cs, pl := loadGuardCandidates()
			switch kind {
			case "rest":
				loadGuardRest(cs[0])
			case "busy":
				busyLoadAt(t, 80)
				plugin.ForgetLoads()
				t.Cleanup(plugin.ForgetLoads)
				cs[0].p = provider.PluginForTest("load-first", "load-plugin")
				plugin.NoteLoadsForTest("load-plugin", map[string]float64{"m": 95}, time.Now())
			case "credits":
				cs[0].p.Account = &provider.Account{Agent: "codex", User: "load@example.com", Plan: "plus"}
				cs[0].model = "gpt-5"
				creditsUsage(t, map[string]float64{"load@example.com": 100}, "load@example.com")
				if err := provider.SetCodexCreditsMode("load@example.com", provider.CreditsLast); err != nil {
					t.Fatal(err)
				}
				if !cs[0].creditsLast(time.Now()) {
					t.Fatal("fixture is not credits-last")
				}
			case "aside":
				pl.order[0].Aside = true
			case "reserve":
				pl.order[0].Reserve = 1
			}
			readings["load-first"] = loadGuardHigh()
			cs, _ = restLast(cs, pl)
			if cs[0].p.ID != "load-second" {
				t.Fatalf("fixture chose %s", cs[0].p.ID)
			}
			if cs[0].loadChosen != (kind == "ready") {
				t.Fatalf("%s predecessor: loadChosen=%v", kind, cs[0].loadChosen)
			}
		})
	}
}

func TestLoadGuardAffinityMissingWeights(t *testing.T) {
	for _, n := range []int{0, 1} {
		t.Run(itoa(n), func(t *testing.T) {
			loadGuardRouting(t)
			cs, pl := loadGuardCandidates()
			body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
			answered(stickKey("group/load", h, body), cs[1], 1, Usage{CacheRead: 5000})
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("missing weights panicked: %v", v)
				}
			}()
			_, _, aff, _ := affine("group/load", provider.AffinityOff, false, false, false, h, provider.Chat, body, body, cs, planned{order: pl.order[:n]})
			if aff.Why != "off" {
				t.Fatalf("affinity: %+v", aff)
			}
		})
	}
}

func TestLoadGuardRotation(t *testing.T) {
	loadGuardRouting(t)
	for _, all := range []bool{false, true} {
		cs, pl := loadGuardCandidates()
		pl.order[1].Loaded = "instance high"
		if all {
			pl.order[0].Loaded, pl.order[2].Loaded = "instance high", "instance high"
		}
		got, _ := after(cs, pl, 0)
		want := "load-third"
		if all {
			want = "load-second"
		}
		if got[0].p.ID != want {
			t.Fatalf("rotation all=%v chose %s, want %s", all, got[0].p.ID, want)
		}
	}
}

func loadGuardRest(c candidate) {
	restingUntil.Lock()
	restingUntil.m[c.restKey()] = time.Now().Add(time.Minute)
	restingUntil.note[c.restKey()] = Rest{Status: 429}
	restingUntil.Unlock()
}

func TestLoadGuardSeatYieldsAfterLaterFailure(t *testing.T) {
	readings := loadGuardRouting(t)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	plan := func() ([]candidate, planned) {
		cs, pl := loadGuardCandidates()
		for i := range cs {
			cs[i].place = 0
		}
		pl.order[1].Fallback, pl.order[2].Fallback = true, true
		return restLast(cs, pl)
	}
	readings["load-first"] = loadGuardHigh()
	cs, pl := plan()
	cs, pl, _, key := affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	answeredStanding(key, cs[0], 1, Usage{CacheRead: 5000}, false)
	clear(readings)
	cs, pl = plan()
	cs, pl, aff, _ := affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	if !aff.Kept || cs[0].p.ID != "load-second" {
		t.Fatalf("load recovery moved seat: %+v", aff)
	}
	answeredStanding(key, cs[0], 2, Usage{CacheRead: 5000}, false)
	sticks.Lock()
	marked := sticks.m[key].loadChosen
	sticks.Unlock()
	if !marked {
		t.Fatal("kept load seat lost its original mark")
	}
	main, _ := loadGuardCandidates()
	loadGuardRest(main[0])
	cs, pl = plan()
	cs, pl, _, _ = affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	answeredStanding(key, cs[0], 3, Usage{CacheRead: 5000}, false)
	restingUntil.Lock()
	delete(restingUntil.m, main[0].restKey())
	restingUntil.Unlock()
	cs, pl = plan()
	cs, _, aff, _ = affine("load-first", "", false, false, false, h, provider.Chat, body, body, cs, pl)
	if cs[0].p.ID != "load-first" || aff.Why != "main" {
		t.Fatalf("load mark outlived a real failure: %s %+v", cs[0].p.ID, aff)
	}
}

func TestLoadGuardMarksOnlyNewSeat(t *testing.T) {
	for _, why := range []string{"first", "gone", "no-cache", "cold", "cache", "session", "strict", "off"} {
		t.Run(why, func(t *testing.T) {
			readings := loadGuardRouting(t)
			cs, pl := loadGuardCandidates()
			body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
			key := stickKey("group/load", h, body)
			mode := ""
			if why != "first" {
				c := cs[1]
				u := Usage{CacheRead: 5000}
				if why == "gone" {
					c.p.ID = "removed"
				}
				if why == "no-cache" {
					u = Usage{}
				}
				answered(key, c, 1, u)
				if why == "cold" {
					sticks.Lock()
					st := sticks.m[key]
					st.at = time.Now().Add(-time.Hour)
					sticks.m[key] = st
					sticks.Unlock()
				}
			}
			if why == "session" || why == "strict" || why == "off" {
				mode = why
			}
			readings["load-first"] = loadGuardHigh()
			cs, pl = restLast(cs, pl)
			cs, _, aff, _ := affine("group/load", mode, false, false, false, h, provider.Chat, body, body, cs, pl)
			if aff.Why != why {
				t.Fatalf("fixture affinity = %s, want %s", aff.Why, why)
			}
			answered(key, cs[0], 1, Usage{CacheRead: 5000})
			sticks.Lock()
			marked := sticks.m[key].loadChosen
			sticks.Unlock()
			want := why == "first" || why == "gone" || why == "no-cache" || why == "cold"
			if marked != want {
				t.Fatalf("%s loadChosen=%v, want %v", why, marked, want)
			}
			for _, c := range cs[1:] {
				if c.loadChosen {
					t.Fatal("unselected fallback marked as load seat")
				}
			}
		})
	}
}

func TestLoadGuardStandInLoadedStays(t *testing.T) {
	readings := loadGuardRouting(t)
	cs, pl := loadGuardCandidates()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{"X-Claude-Code-Session-Id": {t.Name()}}
	key := stickKey("group/load", h, body)
	loadGuardRest(cs[0])
	answeredStanding(key, cs[1], 1, Usage{CacheRead: 5000}, true)
	readings["load-second"] = loadGuardHigh()
	cs, pl = restLast(cs, pl)
	cs, _, aff, _ := affine("group/load", "", false, false, true, h, provider.Chat, body, body, cs, pl)
	if cs[0].p.ID != "load-second" || !aff.Kept {
		t.Fatalf("load moved stand-in while main still rests: %s %+v", cs[0].p.ID, aff)
	}
}
