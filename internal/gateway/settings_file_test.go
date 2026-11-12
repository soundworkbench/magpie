package gateway

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// writeSettingsFile restores the exact file, including its absence, before
// the next test can read it. Callers must not run in parallel.
func writeSettingsFile(t *testing.T, raw []byte) {
	t.Helper()
	path := settings.Path()
	was, err := os.ReadFile(path)
	absent := os.IsNotExist(err)
	if err != nil && !absent {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if absent {
			err = os.Remove(path)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = os.WriteFile(path, was, 0o600)
		}
		if err != nil {
			t.Fatalf("restore settings file: %v", err)
		}
		if _, err := settings.LoadChecked(); err != nil {
			t.Errorf("settings unreadable after restoring the file: %v", err)
		}
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsFileRestoredAfterCorruption(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		absent    bool
	}{
		{name: "existing", raw: "{\n  \"theme\": \"dark\", \"unknown\": true\n}\n"},
		{name: "empty"},
		{name: "absent", absent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if !c.absent {
				writeSettingsFile(t, []byte(c.raw))
			}
			t.Run("corrupt", func(t *testing.T) {
				writeSettingsFile(t, []byte("{not json"))
				if err := settings.Save(settings.Load()); err == nil {
					t.Fatal("save overwrote corrupt settings")
				}
			})
			got, err := os.ReadFile(settings.Path())
			if c.absent {
				if !os.IsNotExist(err) {
					t.Fatalf("settings file should be absent: %v", err)
				}
			} else if err != nil || !bytes.Equal(got, []byte(c.raw)) {
				t.Fatalf("settings file was not restored exactly: %q, %v", got, err)
			}
			if err := settings.Save(settings.Load()); err != nil {
				t.Fatalf("save after corrupting test cleanup: %v", err)
			}
		})
	}
}
