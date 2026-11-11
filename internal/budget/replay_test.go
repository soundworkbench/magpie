package budget

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

type replayResult struct {
	allowed, refused, falseRefusals int
	unpriced                        int
	windows                         map[string]*replayWindow
}

type replayWindow struct {
	limit        access.Limit
	start, reset time.Time
	sums         sums
}

func readReplay(t *testing.T, logPath, limitsPath string) ([]usage.Record, map[string][]access.Limit) {
	t.Helper()
	b, err := os.ReadFile(limitsPath)
	if err != nil {
		t.Fatal(err)
	}
	var limits map[string][]access.Limit
	if err := json.Unmarshal(b, &limits); err != nil {
		t.Fatal(err)
	}
	for id, ls := range limits {
		var clean []access.Limit
		for i := range ls {
			l, err := ls[i].Valid()
			if err != nil {
				t.Fatalf("%s limit %d: %v", id, i, err)
			}
			if l != nil {
				clean = append(clean, *l)
			}
		}
		limits[id] = clean
	}
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []usage.Record
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 16<<20)
	for line := 1; scan.Scan(); line++ {
		var r usage.Record
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		if r.Time.IsZero() || r.Millis < 0 || r.Millis > int64((1<<63-1)/time.Millisecond) {
			t.Fatalf("line %d: invalid time or duration", line)
		}
		records = append(records, r)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return records, limits
}

func replayHome(t *testing.T, records []usage.Record) {
	t.Helper()
	scopedKey(t) // fresh ledger and provider catalog for each run
	// Make recorded provider/model ids resolvable for scoped limits, without
	// contacting any provider. Costing still uses the normal ledger prices.
	models := map[string][]string{}
	for _, r := range records {
		if pid, model, ok := strings.Cut(replayModel(r), "/"); ok && pid != "" && model != "" && !slices.Contains(models[pid], model) {
			models[pid] = append(models[pid], model)
		}
	}
	// For replay only, write temporary providers.json directly, intentionally
	// bypassing provider.Save validation, including for reserved subscription ids.
	// Any request with nonempty parts on both sides of its first slash seeds
	// that prefix as a provider.
	var ps []provider.Provider
	for _, id := range sortedReplayKeys(models) {
		ps = append(ps, provider.Provider{ID: id, Name: id, Key: "replay", Chat: "http://127.0.0.1:1/v1", Models: models[id]})
	}
	b, err := json.Marshal(map[string]any{"providers": ps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provider.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func replayModel(r usage.Record) string {
	if r.Provider != "" && r.Model != "" {
		return r.Provider + "/" + r.Model
	}
	// Local rejections name only the requested model. Keep the record itself
	// unchanged so IsRejected still recognizes rows written without its flag.
	// The replay catalog seeds only provider/model requests, without rebuilding
	// aliases or groups, so unresolved names miss model-scoped limits.
	// A bare model name can still resolve if another record seeded that model.
	return r.Requested
}

// replay admits only starts that Reserve allows. Only admitted requests
// settle, through Append, with their original start stamp and recorded usage.
// Equal-time completions precede new starts; a zero-duration request ends
// immediately after its own start. Late completions stay in their old window.
func replay(t *testing.T, records []usage.Record, limits map[string][]access.Limit, hold bool) map[string]*replayResult {
	t.Helper()
	old := HoldReservations
	HoldReservations = hold
	t.Cleanup(func() { HoldReservations = old })
	type event struct {
		at    time.Time
		index int
		end   bool
	}
	var events []event
	for i, r := range records {
		// These ledger rows never passed through this gateway's limiter.
		// Keep local rejections: replay asks whether they would be admitted.
		if r.IsReported() || r.IsProbe() || r.Computer != "" {
			continue
		}
		events = append(events, event{r.Time, i, false})
		if r.Millis > 0 {
			events = append(events, event{r.Time.Add(time.Duration(r.Millis) * time.Millisecond), i, true})
		}
	}
	slices.SortStableFunc(events, func(a, b event) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		if a.end != b.end {
			if a.end {
				return -1
			}
			return 1
		}
		return a.index - b.index
	})
	results := map[string]*replayResult{}
	identities := map[string]access.Identity{}
	for id, ls := range limits {
		who := access.Identity{KeyID: id, KeyName: id}
		if len(ls) > 0 {
			who.Limit, who.More = &ls[0], ls[1:]
		}
		identities[id] = who
		results[id] = &replayResult{windows: map[string]*replayWindow{}}
	}
	releases := map[int]func(){}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	costOf := usage.NewCoster()
	settle := func(i int) {
		release := releases[i]
		if release == nil {
			return
		}
		r := records[i]
		Append(r)
		release()
		delete(releases, i)
		// Aggregate each original window independently of the current cache,
		// including a request that finishes after the window has reset.
		if !counts(r) {
			return
		}
		cost, priced := costOf(r)
		for _, id := range []string{r.CallerKeyID, r.CallerIssuerID} {
			result := results[id]
			if result == nil {
				continue
			}
			for n, l := range limits[id] {
				start, reset := l.Window(r.Time)
				if r.Time.Before(start) || l.Scoped() && !access.ModelAllowed(scopeModels(&l), r.Provider+"/"+r.Model) {
					continue
				}
				key := fmt.Sprintf("%d/%s", n, start.Format(time.RFC3339Nano))
				w := result.windows[key]
				if w == nil {
					w = &replayWindow{limit: l, start: start, reset: reset}
					result.windows[key] = w
				}
				w.sums.add(r, cost, priced)
			}
		}
	}
	for _, e := range events {
		if e.end {
			settle(e.index)
			continue
		}
		r := records[e.index]
		who := identities[r.CallerKeyID]
		result := results[r.CallerKeyID]
		if result == nil { // no configured cap, still report this key
			who = access.Identity{KeyID: r.CallerKeyID, KeyName: r.CallerKeyID}
			result = &replayResult{windows: map[string]*replayWindow{}}
			results[r.CallerKeyID] = result
		}
		if issuer, ok := identities[r.CallerIssuerID]; ok {
			who.Issuer = &issuer
		}
		model := replayModel(r)
		// Check actual sums independently of held, across every applicable cap;
		// a different, later-reset reservation refusal mustn't hide a spent cap.
		actuallySpent := false
		ids := []access.Identity{who}
		if who.Issuer != nil {
			ids = append(ids, *who.Issuer)
		}
		_, priced := costOf(r)
		for _, id := range ids {
			unpriced := false
			for n, l := range limits[id.KeyID] {
				if l.Scoped() && !scopeHas(&l, servedAs(model)) {
					continue
				}
				unpriced = unpriced || l.Cost > 0 && !priced
				start, _ := l.Window(e.at)
				w := results[id.KeyID].windows[fmt.Sprintf("%d/%s", n, start.Format(time.RFC3339Nano))]
				if w == nil {
					continue
				}
				actuallySpent = actuallySpent || l.Tokens > 0 && w.sums.tokens(&l) >= l.Tokens || l.Cost > 0 && w.sums.cost >= l.Cost
			}
			// Count each input row once per key, even if refused or covered by
			// several cost caps. Settlement alone misses refused rows.
			if unpriced {
				results[id.KeyID].unpriced++
			}
		}
		release, refused := Reserve(who, int64(r.PlainBytes), model, e.at)
		if refused != nil {
			result.refused++
			if !actuallySpent {
				result.falseRefusals++
			}
		} else {
			result.allowed++
			releases[e.index] = release
		}
		if r.Millis == 0 {
			settle(e.index)
		}
	}
	for _, id := range sortedReplayKeys(results) {
		r := results[id]
		var excess []string
		for _, key := range sortedReplayKeys(r.windows) {
			w := r.windows[key]
			var tokens int64
			var cost float64
			if w.limit.Tokens > 0 {
				tokens = max(w.sums.tokens(&w.limit)-w.limit.Tokens, 0)
			}
			if w.limit.Cost > 0 {
				cost = max(w.sums.cost-w.limit.Cost, 0)
			}
			excess = append(excess, fmt.Sprintf("%s..%s limit=%s/%s excess_tokens=%d excess_cost=$%.6f unpriced=%d", w.start.Format(time.RFC3339), w.reset.Format(time.RFC3339), w.limit.Period, w.limit.Scope(), tokens, cost, w.sums.unpriced))
		}
		t.Logf("hold=%t key=%s allowed=%d refused=%d false_refusals=%d unpriced=%d windows=[%s]", hold, id, r.allowed, r.refused, r.falseRefusals, r.unpriced, strings.Join(excess, "; "))
		if !hold && r.falseRefusals != 0 {
			t.Errorf("key %s: %d false refusals", id, r.falseRefusals)
		}
	}
	state.Lock()
	n := len(state.held)
	state.Unlock()
	if n != 0 {
		t.Errorf("replay left %d held windows", n)
	}
	return results
}

func sortedReplayKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestBudgetReplay(t *testing.T) {
	logPath, limitsPath := os.Getenv("MAGPIE_BUDGET_REPLAY"), os.Getenv("MAGPIE_BUDGET_REPLAY_LIMITS")
	if logPath == "" || limitsPath == "" {
		t.Skip("set MAGPIE_BUDGET_REPLAY and MAGPIE_BUDGET_REPLAY_LIMITS")
	}
	records, limits := readReplay(t, logPath, limitsPath)
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprint(hold), func(t *testing.T) {
			replayHome(t, records)
			results := replay(t, records, limits, hold)
			for _, id := range sortedReplayKeys(results) {
				if n := results[id].unpriced; n > 0 {
					t.Errorf("key %s: %d records with unknown cost in cost-limited scopes", id, n)
				}
			}
		})
	}
}

func TestBudgetReplaySample(t *testing.T) {
	records, limits := readReplay(t, "testdata/replay-sample.jsonl", "testdata/replay-sample-limits.json")
	// UTC makes the midnight-crossing sample independent of the test host.
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprint(hold), func(t *testing.T) {
			replayHome(t, records)
			input, zero := 1000.0, 0.0
			if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"plan/m1": {Input: &input, Output: &zero, CacheRead: &zero, CacheWrite: &zero}}}); err != nil {
				t.Fatal(err)
			}
			results := replay(t, records, limits, hold)
			for id, r := range results {
				if r.unpriced != 0 {
					t.Errorf("key %s: unpriced=%d want 0", id, r.unpriced)
				}
			}
			want := [3]int{4, 1, 0}
			if hold {
				want = [3]int{2, 3, 3}
			}
			a := results["key-a"]
			if got := [3]int{a.allowed, a.refused, a.falseRefusals}; got != want {
				t.Fatalf("key-a: %v want %v", got, want)
			}
			for _, w := range a.windows {
				wantCost := 36.0
				if hold {
					wantCost = 24
				}
				if w.sums.cost != wantCost {
					t.Fatalf("settled cost %v want %v", w.sums.cost, wantCost)
				}
			}
			b := results["key-b"]
			want = [3]int{4, 1, 0}
			if hold {
				want = [3]int{4, 1, 1}
			}
			if got := [3]int{b.allowed, b.refused, b.falseRefusals}; got != want {
				t.Fatalf("key-b: %v want %v", got, want)
			}
			if len(b.windows) != 2 {
				t.Fatalf("cross-midnight windows: %d", len(b.windows))
			}
			for _, w := range b.windows {
				wantTokens := int64(600)
				if w.start.Day() == 1 {
					wantTokens = 1100
					if hold {
						wantTokens = 701
					}
				}
				if got := w.sums.tokens(&w.limit); got != wantTokens {
					t.Fatalf("window %s: %d tokens want %d", w.start, got, wantTokens)
				}
			}
			c := results["key-c"]
			if got := [3]int{c.allowed, c.refused, c.falseRefusals}; got != [3]int{2, 2, 0} {
				t.Fatalf("key-c: %v want [2 2 0]", got)
			}
			if len(c.windows) != 1 {
				t.Fatalf("subscription windows: %d want 1", len(c.windows))
			}
			for _, w := range c.windows {
				if w.sums.cost != 12 {
					t.Fatalf("recorded cost %v want 12", w.sums.cost)
				}
			}
			d := results["key-d"]
			if got := [3]int{d.allowed, d.refused, d.falseRefusals}; got != [3]int{1, 1, 0} {
				t.Fatalf("key-d: %v want [1 1 0]", got)
			}
		})
	}
}

func TestBudgetReplayInputs(t *testing.T) {
	t.Run("normalized limits", func(t *testing.T) {
		path := t.TempDir() + "/limits.json"
		if err := os.WriteFile(path, []byte(`{"key-a":[{"tokens":1000,"models":[" plan/m1 "]}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, ls := readReplay(t, "testdata/replay-sample.jsonl", path)
		if l := ls["key-a"][0]; l.Period != "day" || len(l.Models) != 1 || l.Models[0] != "plan/m1" {
			t.Fatalf("limit not normalized: %+v", l)
		}
	})
	for _, source := range []string{usage.SourceReported, usage.SourceProbe, usage.SourceMCP, usage.SourceSearch, "computer"} {
		t.Run(source, func(t *testing.T) {
			at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			observed := usage.Record{Time: at, Millis: 10000, CallerKeyID: "key-a", Provider: "plan", Model: "m1", PlainBytes: 8000, Input: 2000, Status: 200, Source: source}
			if source == "computer" {
				observed.Source, observed.Computer = "", "remote"
			}
			records := []usage.Record{observed, {Time: at.Add(time.Second), CallerKeyID: "key-a", Provider: "plan", Model: "m1", Input: 100, Status: 200}}
			replayHome(t, records)
			got := replay(t, records, map[string][]access.Limit{"key-a": {{Period: "day", Tokens: 1000}}}, true)["key-a"]
			if got.allowed != 1 || got.refused != 0 || got.falseRefusals != 0 {
				t.Fatalf("non-request acquired a reservation: %+v", got)
			}
		})
	}
}

func TestBudgetReplayUnpriced(t *testing.T) {
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprint(hold), func(t *testing.T) {
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			one, zero := 1.0, 0.0
			records := []usage.Record{
				{Time: at, CallerKeyID: "key-a", Provider: "plan", Model: "unlisted-model", Input: 100, Status: 200},
				{Time: at.Add(time.Second), CallerKeyID: "key-a", Provider: "plan", Model: "unlisted-model", Input: 100, OwnCost: &one, Status: 200},
				// This unpriced call is refused, but still makes the input incomplete.
				{Time: at.Add(2 * time.Second), CallerKeyID: "key-a", CallerIssuerID: "key-c", Provider: "plan", Model: "unlisted-model", Input: 100, Status: 200},
				{Time: at.Add(3 * time.Second), CallerKeyID: "key-a", Provider: "outside", Model: "unlisted-model", Input: 100, Status: 200},
				{Time: at.Add(4 * time.Second), CallerKeyID: "key-a", Provider: "plan", Model: "unlisted-model", Input: 100, OwnCost: &zero, Status: 200},
				{Time: at, CallerKeyID: "key-b", Provider: "plan", Model: "unlisted-model", Input: 100, Status: 200},
			}
			limits := map[string][]access.Limit{
				"key-a": {{Period: "day", Cost: 1, Models: []string{"plan/*"}}, {Period: "month", Cost: 10, Models: []string{"plan/*"}}},
				"key-b": {{Period: "day", Tokens: 1000}},
				"key-c": {{Period: "day", Cost: 1, Models: []string{"plan/*"}}},
			}
			replayHome(t, records)
			results := replay(t, records, limits, hold)
			if got := results["key-a"].refused; got != 2 {
				t.Errorf("key-a: refused=%d want 2", got)
			}
			for id, want := range map[string]int{"key-a": 2, "key-b": 0, "key-c": 1} {
				if got := results[id].unpriced; got != want {
					t.Errorf("key %s: unpriced=%d want %d", id, got, want)
				}
			}
		})
	}
}
