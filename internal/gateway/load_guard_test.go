package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func loadGuardFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadGuardMemory(t *testing.T) {
	root := loadGuardFiles(t, map[string]string{
		"proc/meminfo":                         "MemTotal: 8388608 kB\nMemAvailable: 3145728 kB\nMemFree: 100 kB\n",
		"proc/self/cgroup":                     "0::/service\n",
		"sys/fs/cgroup/service/memory.current": "1073741824\n",
		"sys/fs/cgroup/service/memory.stat":    "anon 536870912\ninactive_file 268435456\nactive_file 123\n",
		"sys/fs/cgroup/service/memory.max":     "2147483648\n",
	})
	r := loadReading("linux", root)
	if r.HostTotalMiB != 8192 || r.HostAvailableMiB != 3072 || r.SelfMiB != 768 || r.SelfLimitMiB != 2048 || r.At <= 0 {
		t.Fatalf("memory snapshot: %+v", r)
	}
	if err := os.WriteFile(filepath.Join(root, "sys/fs/cgroup/service/memory.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := loadReading("linux", root); r.SelfLimitMiB != 0 {
		t.Fatalf("unlimited: %+v", r)
	}
	for _, goos := range []string{"darwin", "windows"} {
		r := loadReading(goos, root)
		if r.HostTotalMiB != -1 || r.HostAvailableMiB != -1 || r.SelfMiB != -1 || r.SelfLimitMiB != -1 {
			t.Fatalf("%s: %+v", goos, r)
		}
	}
}

func TestLoadGuardMemoryRSS(t *testing.T) {
	pid := os.Getpid()
	root := loadGuardFiles(t, map[string]string{
		fmt.Sprintf("proc/%d/status", pid): fmt.Sprintf("Name: magpie\nPPid: 1\nVmRSS: 1024 kB\n"),
		"proc/900001/status":               fmt.Sprintf("PPid: %d\nVmRSS: 2048 kB\n", pid),
		"proc/900002/status":               "PPid: 900001\nVmRSS: 4096 kB\n",
		"proc/900003/status":               "PPid: 1\nVmRSS: 8192 kB\n",
	})
	r := loadReading("linux", root)
	if r.SelfMiB != 7 || r.HostAvailableMiB != -1 || r.HostTotalMiB != -1 || r.SelfLimitMiB != 0 {
		t.Fatalf("RSS tree: %+v", r)
	}
	if r := loadReading("linux", t.TempDir()); r.SelfMiB != -1 {
		t.Fatalf("missing memory is not unknown: %+v", r)
	}
}

func TestLoadGuardEndpoint(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	keys, secrets := newCaller(t, "Load reader")
	if _, err := access.Update("access-key", access.Change{Key: keys[0].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	handler := lanGuard(New().Handler())
	for _, tc := range []struct {
		key  string
		code int
	}{{secrets[0], http.StatusOK}, {"", http.StatusUnauthorized}} {
		r := httptest.NewRequest("GET", provider.RemoteLoadPath, nil)
		r.RemoteAddr = "192.168.1.9:5000"
		if tc.key != "" {
			r.Header.Set("Authorization", "Bearer "+tc.key)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("load endpoint status %d, want %d: %s", w.Code, tc.code, w.Body.String())
		}
		if w.Code == http.StatusOK {
			var got provider.LoadReading
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.At <= 0 {
				t.Fatalf("load body: %s (%v)", w.Body.String(), err)
			}
		}
	}
}

func TestLoadGuardMemoryUnknown(t *testing.T) {
	for _, stat := range []string{"", "inactive_file bad\n", "inactive_file -1\n"} {
		root := loadGuardFiles(t, map[string]string{
			"proc/meminfo":                         "MemTotal: bad kB\nMemAvailable: -1 kB\n",
			"proc/self/cgroup":                     "0::/service\n",
			"sys/fs/cgroup/service/memory.current": "1073741824\n",
			"sys/fs/cgroup/service/memory.stat":    stat,
			"sys/fs/cgroup/service/memory.max":     "bad",
		})
		if r := loadReading("linux", root); r.HostTotalMiB != -1 || r.HostAvailableMiB != -1 || r.SelfMiB != -1 || r.SelfLimitMiB != -1 {
			t.Fatalf("malformed memory became a reading: %+v", r)
		}
	}
}

func TestLoadGuardEndpointIdleCounts(t *testing.T) {
	h := newResumeHarness(t)
	loadGuardIdleConfig(t, settings.LoadGuardConfig{})
	h.ask("rules", "first")
	w := httptest.NewRecorder()
	h.s.load(w, httptest.NewRequest("GET", provider.RemoteLoadPath, nil))
	var got provider.LoadReading
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ClaudeRuns != 1 || got.ClaudeIdle != 1 {
		t.Fatalf("process counts: %+v", got)
	}
	loadGuardIdleConfig(t, settings.LoadGuardConfig{Off: true})
	w = httptest.NewRecorder()
	h.s.load(w, httptest.NewRequest("GET", provider.RemoteLoadPath, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("off endpoint: %d", w.Code)
	}
}
