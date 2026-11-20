package settings

import "testing"

func TestWarmWaitSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := WarmWaitNow(); got.Off || got.Seconds != 30 {
		t.Fatalf("missing setting: %+v", got)
	}
	for _, c := range []struct{ raw, want int }{
		{0, 30}, {-1, 20}, {1, 20}, {19, 20}, {20, 20}, {30, 30}, {60, 60}, {61, 60},
	} {
		s := Load()
		s.WarmWait = WarmWaitConfig{Off: true, Seconds: c.raw}
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
		if got := WarmWaitNow(); !got.Off || got.Seconds != c.want {
			t.Fatalf("seconds %d: %+v, want off and %d", c.raw, got, c.want)
		}
	}
	// A subsequent save takes effect without restarting the reader.
	if err := Save(Settings{}); err != nil {
		t.Fatal(err)
	}
	if got := WarmWaitNow(); got.Off || got.Seconds != 30 {
		t.Fatalf("reenabled: %+v", got)
	}
}
