package settings

import "testing"

func TestLoadGuardSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := LoadGuardConfig{HostFreeMinMiB: 2048, InstanceMaxMiB: 2560, IdleProcessMinutes: 30}
	if got := LoadGuardNow(); got != want {
		t.Fatalf("missing key: got %+v, want %+v", got, want)
	}
	for _, raw := range []LoadGuardConfig{
		{},
		{Off: true},
		{HostFreeMinMiB: 100},
		{InstanceMaxMiB: 200},
		{IdleProcessMinutes: 7},
		{HostFreeMinMiB: -1, InstanceMaxMiB: -2, IdleProcessMinutes: -3},
		{HostFreeMinMiB: 10, InstanceMaxMiB: 20, IdleProcessMinutes: 5},
	} {
		s := Load()
		s.LoadGuard = raw
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
		expected := want
		expected.Off = raw.Off
		if raw.HostFreeMinMiB > 0 {
			expected.HostFreeMinMiB = raw.HostFreeMinMiB
		}
		if raw.InstanceMaxMiB > 0 {
			expected.InstanceMaxMiB = raw.InstanceMaxMiB
		}
		if raw.IdleProcessMinutes > 0 {
			expected.IdleProcessMinutes = raw.IdleProcessMinutes
		}
		if got := LoadGuardNow(); got != expected {
			t.Fatalf("hot settings %+v: got %+v, want %+v", raw, got, expected)
		}
	}
}

func TestLoadGuardKeepOwn(t *testing.T) {
	own := LoadGuardConfig{Off: true, HostFreeMinMiB: 123, InstanceMaxMiB: 456, IdleProcessMinutes: 7}
	for _, want := range []LoadGuardConfig{own, {}} {
		s := Settings{LoadGuard: LoadGuardConfig{IdleProcessMinutes: 99}}
		s.KeepOwn(Settings{LoadGuard: want})
		if s.LoadGuard != want {
			t.Fatalf("sync changed local load guard: got %+v, want %+v", s.LoadGuard, want)
		}
	}
}
