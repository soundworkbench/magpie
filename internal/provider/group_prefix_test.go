package provider

import (
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

func TestPrefixedGroupFor(t *testing.T) {
	azureHome(t)
	const model = "claude-opus-5-5"
	// Save requires a key unless Ready would accept a keyless provider.
	// Store a record without its key to exercise an existing, unready provider.
	if err := store(file{Providers: []Provider{{ID: "unready", Name: "Unready", Chat: "https://relay.example.com/v1"}}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Provider{
		{ID: "remote-a", Name: "Remote A", Models: []string{"claude/" + model}},
		{ID: "vendor", Name: "Friendly Vendor", Was: []string{"old"}, Models: []string{model}},
		{ID: "disabled", Name: "Disabled", Was: []string{"old-disabled"}, Off: true, Models: []string{model}},
	} {
		p.Key, p.Chat = "k", "http://127.0.0.1:1/v1"
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveGroup(Group{ID: model, Members: []string{"remote-a/claude/" + model}}); err != nil {
		t.Fatal(err)
	}
	if p := lookIn(sharedAll(), "unready"); p == nil || p.Ready() || p.On() || p.Off || p.Key != "" {
		t.Fatal("expected the keyless provider to exist without being ready, enabled or switched off")
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"claude/" + model, true},
		{"unknown/" + model, true},
		{"unready/" + model, true},
		{"disabled/" + model, false},
		{"old-disabled/" + model, false},
		{" claude/" + model + "[1m] ", true},
		{"vendor/" + model, false},
		{"old/" + model, false},
		{"FRIENDLY VENDOR/" + model, false},
		{"remote-a/claude/" + model, false},
		{"unknown/claude/" + model, false},
		{"group/" + model, false},
		{"claude/no-group", false},
		{model, false},
		{"/" + model, false},
		{"claude/", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got, ok := PrefixedGroupFor(tc.id)
			if ok != tc.want || ok && got != GroupPrefix+model {
				t.Fatalf("PrefixedGroupFor(%q) = %q, %v; want group = %v", tc.id, got, ok, tc.want)
			}
		})
	}
	// Resolving a provider model remains independent of this optional
	// gateway rule; the key can still fall back to the original request.
	if p, m, ok := Resolve("claude/" + model); !ok || p.ID != "remote-a" || m != "claude/"+model {
		t.Fatalf("Resolve changed: %s/%s, %v", p.ID, m, ok)
	}
	if _, ok := GroupFor("claude/" + model); ok {
		t.Fatal("GroupFor must still leave prefixed ids to its caller")
	}
}

func TestPrefixedGroupForSwitchedOffProvider(t *testing.T) {
	azureHome(t)
	const model = "shared-model"
	for _, id := range []string{"remote-a", "remote-b"} {
		if err := Save(Provider{ID: id, Name: id, Key: "test-key", Chat: "http://127.0.0.1:1/v1", Models: []string{model}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetOff("remote-b", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := GroupFor(model); !ok {
		t.Fatal("expected a group for the shared model after switching one provider off")
	}
	for _, id := range []string{"remote-b/" + model, "REMOTE-B/" + model, " remote-b/" + model + "[1m] "} {
		if got, ok := PrefixedGroupFor(id); ok || got != "" {
			t.Errorf("PrefixedGroupFor(%q) = %q, %v; want no group", id, got, ok)
		}
	}
	if p, m, ok := Resolve("remote-a/" + model); !ok || p.ID != "remote-a" || m != model {
		t.Fatalf("enabled provider resolved to %s/%s, %v", p.ID, m, ok)
	}
}

func TestPrefixedGroupForUnavailableProviders(t *testing.T) {
	home := claudeHome(t)
	claudeSignIn(t, home, time.Now().Add(time.Hour))
	if err := SetSwitchOnly("claude", true); err != nil {
		t.Fatal(err)
	}
	const model = "shared-model"
	for _, p := range []Provider{
		{ID: "remote-a", Key: "test-key", Chat: "http://127.0.0.1:1/v1", Models: []string{model}},
		{ID: "remote-self", Preset: RemoteMagpiePreset, Key: "test-key", Chat: "http://127.0.0.1:1/v1"},
	} {
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	self, err := Find("remote-self")
	if err != nil {
		t.Fatal(err)
	}
	noteSelf(*self, []catalog.Model{{ID: model, Via: []string{catalog.Self()}}})
	if err := SaveGroup(Group{ID: model, Members: []string{"remote-a/" + model}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := GroupFor(model); !ok {
		t.Fatal("expected a group for the shared model")
	}
	for _, tc := range []struct {
		id    string
		state func(Provider) bool
	}{
		{"claude", Provider.KeptToAgent},
		{"remote-self", Provider.SelfLeftOut},
	} {
		t.Run(tc.id, func(t *testing.T) {
			p, err := Find(tc.id)
			if err != nil || !tc.state(*p) || p.On() || p.Off {
				t.Fatal("expected a provider kept out of service without being switched off")
			}
			id := tc.id + "/" + model
			if off, ok := SwitchedOff(id); !ok || off.ID != tc.id {
				t.Fatalf("SwitchedOff(%q) = %q, %v; want the named provider", id, off.ID, ok)
			}
			if got, ok := PrefixedGroupFor(id); ok || got != "" {
				t.Fatalf("PrefixedGroupFor(%q) = %q, %v; want no group", id, got, ok)
			}
		})
	}
}
