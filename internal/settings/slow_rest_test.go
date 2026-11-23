package settings

import (
	"testing"
	"time"
)

func TestSlowRestDefaultsAndInvalidValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := Defaults().SlowRestNow(); got.Forget != 30*time.Minute || got.Rests != 2 || got.Cooldown != time.Minute {
		t.Fatalf("defaults: %+v", got)
	}
	for _, field := range []struct {
		name string
		set  func(*Settings, int)
	}{
		{name: "forget", set: func(s *Settings, n int) { s.SlowForget = n }},
		{name: "rests", set: func(s *Settings, n int) { s.SlowRests = n }},
		{name: "cooldown", set: func(s *Settings, n int) { s.SlowCooldown = n }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, bad := range []int{0, -1} {
				s := Settings{SlowForget: SlowForgetDefault, SlowRests: SlowRestsDefault, SlowCooldown: SlowCooldownDefault}
				field.set(&s, bad)
				if err := Save(s); err != nil {
					t.Fatal(err)
				}
				if got := Load().SlowRestNow(); got.Forget != 30*time.Minute || got.Rests != 2 || got.Cooldown != time.Minute {
					t.Fatalf("invalid %d: %+v", bad, got)
				}
			}
		})
	}
}
