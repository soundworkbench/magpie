package budget

import (
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

func TestActualOnlyReservations(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "whole", true: "models"}[scoped], func(t *testing.T) {
			lim := access.Limit{Period: "day", Tokens: 1000, Cost: 30}
			if scoped {
				lim.Models = []string{"plan/m1"}
			}
			who := scopedKey(t, lim)
			input, zero := 60_000.0, 0.0
			if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
			Append(usage.Record{Time: now, CallerKeyID: who.KeyID, Provider: "plan", Model: "m1", Input: 300, Status: 200})
			for range 3 {
				release, refused := Reserve(who, 2<<20, "plan/m1", now)
				if refused != nil {
					t.Fatal("reservation refused below actual cap:", refused)
				}
				t.Cleanup(release)
			}
			if spent := Spent(who, now); len(spent) != 0 {
				t.Fatal("reservations removed candidates:", spent)
			}
			st := Of(access.Key{ID: who.KeyID, Limit: who.Limit}, now)
			if st.InFlight != 3 || st.Reserved != 3*(2<<20)/4 || st.Cost != 18 || st.ReservedCost <= 30 {
				t.Fatalf("status %+v", st)
			}
			Append(usage.Record{Time: now, CallerKeyID: who.KeyID, Provider: "plan", Model: "m1", Input: 200, Status: 200})
			release, refused := Reserve(who, 4, "plan/m1", now)
			if release != nil {
				release()
			}
			if refused == nil || !strings.Contains(refused.Error(), "$30.00 of its $30.00") || strings.Contains(refused.Error(), "in flight") {
				t.Fatalf("actual cost refusal: %v", refused)
			}
			if scoped && len(Spent(who, now)) != 1 {
				t.Fatal("settled scope was not spent")
			}
		})
	}
}

func TestActualOnlyIssuedLimits(t *testing.T) {
	for _, cap := range []string{"tokens", "cost"} {
		for _, exhausted := range []string{"own", "issuer"} {
			t.Run(cap+"/"+exhausted, func(t *testing.T) {
				who := scopedKey(t, access.Limit{Period: "day", Tokens: 1000})
				who.Issuer = &access.Identity{KeyID: "issuer", KeyName: "Issuer", Limit: &access.Limit{Period: "day", Tokens: 2000}}
				if cap == "cost" {
					who.Limit.Tokens, who.Limit.Cost = 0, 1
					who.Issuer.Limit.Tokens, who.Issuer.Limit.Cost = 0, 2
					input, zero := 1000.0, 0.0
					if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
				key := func(id access.Identity) access.Key { return access.Key{ID: id.KeyID, Limit: id.Limit} }
				var releases []func()
				for range 3 {
					release, refused := Reserve(who, 2<<20, "plan/m1", now)
					if refused != nil {
						t.Fatal("held issued key refused:", refused)
					}
					t.Cleanup(release)
					releases = append(releases, release)
				}
				for _, id := range []access.Identity{who, *who.Issuer} {
					if st := Of(key(id), now); st.InFlight != 3 {
						t.Fatalf("issued holds: %+v", st)
					}
				}
				for _, release := range releases {
					release()
				}
				rec := usage.Record{Time: now, CallerKeyID: who.KeyID, CallerIssuerID: who.Issuer.KeyID, Provider: "plan", Model: "m1", Input: 1000, Status: 200}
				want := who.KeyName
				if exhausted == "issuer" {
					rec.CallerKeyID, rec.Input, want = "sibling", 2000, who.Issuer.KeyName
				}
				Append(rec)
				release, refused := Reserve(who, 4, "plan/m1", now)
				if release != nil {
					release()
				}
				if refused == nil || refused.Key != want {
					t.Fatalf("want %s refusal, got %v", want, refused)
				}
				for _, id := range []access.Identity{who, *who.Issuer} {
					st := Of(key(id), now)
					if st.InFlight != 0 || st.Reserved != 0 || st.ReservedCost != 0 {
						t.Fatalf("refusal leaked: %+v", st)
					}
				}
			})
		}
	}
}

func TestHoldReservationsRestoresAdmission(t *testing.T) {
	old := HoldReservations
	HoldReservations = true
	t.Cleanup(func() { HoldReservations = old })
	for _, cap := range []string{"tokens", "cost"} {
		t.Run(cap, func(t *testing.T) {
			lim := access.Limit{Period: "day", Tokens: 1000, Models: []string{"plan/m1"}}
			if cap == "cost" {
				lim.Tokens, lim.Cost = 0, 30
			}
			who := scopedKey(t, lim)
			input, zero := 60_000.0, 0.0
			if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
			release, refused := Reserve(who, 8000, "plan/m1", now)
			if refused != nil {
				t.Fatal(refused)
			}
			t.Cleanup(release)
			second, refused := Reserve(who, 4, "plan/m1", now)
			if second != nil {
				second()
			}
			if refused == nil || refused.Spent || !strings.Contains(refused.Error(), "with 1 requests in flight holding the rest") {
				t.Fatalf("old refusal: %v", refused)
			}
			if len(Spent(who, now)) != 1 {
				t.Fatal("reserved model not excluded")
			}
			release()
			next, refused := Reserve(who, 4, "plan/m1", now)
			if refused != nil {
				t.Fatal("released reservation still blocks:", refused)
			}
			next()
			st := Of(access.Key{ID: who.KeyID, Limit: who.Limit}, now)
			if st.InFlight != 0 || st.Reserved != 0 || st.ReservedCost != 0 {
				t.Fatalf("leaked %+v", st)
			}
		})
	}
}
