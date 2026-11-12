package provider

import "testing"

func TestPrefixedGroupFor(t *testing.T) {
	azureHome(t)
	const model = "claude-opus-5-5"
	for _, p := range []Provider{
		{ID: "rm-q", Name: "Remote Q", Models: []string{"claude/" + model}},
		{ID: "vendor", Name: "Friendly Vendor", Was: []string{"old"}, Models: []string{model}},
		{ID: "disabled", Name: "Disabled", Was: []string{"old-disabled"}, Off: true, Models: []string{model}},
	} {
		p.Key, p.Chat = "k", "http://127.0.0.1:1/v1"
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveGroup(Group{ID: model, Members: []string{"rm-q/claude/" + model}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"claude/" + model, true},
		{"unknown/" + model, true},
		{"disabled/" + model, true},
		{"old-disabled/" + model, true},
		{" claude/" + model + "[1m] ", true},
		{"vendor/" + model, false},
		{"old/" + model, false},
		{"FRIENDLY VENDOR/" + model, false},
		{"rm-q/claude/" + model, false},
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
	if p, m, ok := Resolve("claude/" + model); !ok || p.ID != "rm-q" || m != "claude/"+model {
		t.Fatalf("Resolve changed: %s/%s, %v", p.ID, m, ok)
	}
	if _, ok := GroupFor("claude/" + model); ok {
		t.Fatal("GroupFor must still leave prefixed ids to its caller")
	}
}
