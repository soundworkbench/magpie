package settingsapi

import (
	"github.com/yetone/magpie/internal/settings"
	"testing"
)

func TestLoadGuardPageSaveKeepsOwn(t *testing.T) {
	home(t)
	for _, guard := range []settings.LoadGuardConfig{{Off: true, HostFreeMinMiB: 123, InstanceMaxMiB: 456, IdleProcessMinutes: 7}, {}} {
		cur := settings.Defaults()
		cur.LoadGuard = guard
		for _, body := range []string{`{"theme":"dark"}`, `{"loadGuard":{"off":false,"hostFreeMinMiB":1,"idleProcessMinutes":99}}`} {
			in, err := Merge([]byte(body), cur, false)
			if err != nil {
				t.Fatal(err)
			}
			if in.LoadGuard != guard {
				t.Fatalf("page changed machine's load guard: got %+v, want %+v", in.LoadGuard, guard)
			}
		}
	}
}
