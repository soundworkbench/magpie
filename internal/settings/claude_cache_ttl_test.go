package settings

import (
	"reflect"
	"testing"
)

func TestClaudeCacheTTLSettingsSync(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := ClaudeCacheTTLConfig{TTL: "1h", Groups: []string{"claude-opus-*", "group/*"}, Keys: []string{"key-id", "Team"}, ExceptKeys: []string{"except"}}
	s := Settings{WarmWait: WarmWaitConfig{Seconds: 45}, ClaudeCacheTTL: want}
	s.KeepOwn(Settings{WarmWait: WarmWaitConfig{Off: true}})
	if s.WarmWait.Off || s.WarmWait.Seconds != 45 || !reflect.DeepEqual(s.ClaudeCacheTTL, want) {
		t.Fatalf("sync replaced routing settings: %+v / %+v", s.WarmWait, s.ClaudeCacheTTL)
	}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if got := Shared().ClaudeCacheTTL; !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v", got)
	}
	s.ClaudeCacheTTL = ClaudeCacheTTLConfig{}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if got := Shared().ClaudeCacheTTL; got.TTL != "" {
		t.Fatalf("disabled policy stayed cached: %+v", got)
	}
}
