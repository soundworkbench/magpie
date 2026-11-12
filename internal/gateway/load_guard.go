package gateway

import (
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	if settings.LoadGuardNow().Off {
		http.NotFound(w, r)
		return
	}
	reading := loadReading(memGOOS, memRoot)
	if b := s.subscription; b != nil {
		b.mu.Lock()
		for _, run := range b.runs {
			run.mu.Lock()
			live := !run.closed
			run.mu.Unlock()
			if live {
				reading.ClaudeRuns++
				if run.idleKey != "" && b.idle[run.idleKey] == run {
					reading.ClaudeIdle++
				}
			}
		}
		b.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, struct {
		provider.LoadReading
		Remotes []provider.RemoteLoadStatus `json:"remotes,omitempty"`
	}{reading, provider.RemoteLoadStatuses()})
}

func loadReading(goos, root string) provider.LoadReading {
	r := provider.LoadReading{At: time.Now().Unix(), HostAvailableMiB: -1, HostTotalMiB: -1, SelfMiB: -1, SelfLimitMiB: -1}
	if goos != "linux" {
		return r
	}
	info, _ := os.ReadFile(filepath.Join(root, "proc/meminfo"))
	if n, ok := memoryField(info, "MemTotal:"); ok {
		r.HostTotalMiB = n / 1024
	}
	if n, ok := memoryField(info, "MemAvailable:"); ok {
		r.HostAvailableMiB = n / 1024
	}
	r.SelfLimitMiB = 0
	groups, _ := os.ReadFile(filepath.Join(root, "proc/self/cgroup"))
	for _, line := range strings.Split(string(groups), "\n") {
		if dir, ok := strings.CutPrefix(line, "0::"); ok {
			base := filepath.Join(root, "sys/fs/cgroup", path.Clean("/"+dir))
			r.SelfLimitMiB = -1
			if limit, err := os.ReadFile(filepath.Join(base, "memory.max")); err == nil {
				value := strings.TrimSpace(string(limit))
				if value == "max" {
					r.SelfLimitMiB = 0
				} else if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
					r.SelfLimitMiB = n >> 20
				}
			}
			current, err := os.ReadFile(filepath.Join(base, "memory.current"))
			stat, statErr := os.ReadFile(filepath.Join(base, "memory.stat"))
			inactive, known := memoryField(stat, "inactive_file")
			n, parseErr := strconv.ParseInt(strings.TrimSpace(string(current)), 10, 64)
			if err == nil && statErr == nil && known && parseErr == nil && n >= 0 {
				r.SelfMiB = max(n-inactive, 0) >> 20
			}
			break
		}
	}
	if r.SelfMiB < 0 {
		r.SelfMiB = processTreeMiB(root, os.Getpid())
	}
	return r
}

func memoryField(b []byte, name string) (int64, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == name {
			n, err := strconv.ParseInt(f[1], 10, 64)
			return n, err == nil && n >= 0
		}
	}
	return 0, false
}

// processTreeMiB sums the process and every descendant, independent of
// directory order. Processes that disappear or deny access are skipped;
// a missing root or malformed RSS in a readable descendant is unknown.
func processTreeMiB(root string, pid int) int64 {
	return processTreeMiBRead(root, pid, os.ReadFile)
}

func processTreeMiBRead(root string, pid int, readFile func(string) ([]byte, error)) int64 {
	type process struct {
		parent int
		rss    int64
		known  bool
	}
	ps := map[int]process{}
	entries, err := os.ReadDir(filepath.Join(root, "proc"))
	if err != nil {
		return -1
	}
	for _, entry := range entries {
		id, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		b, err := readFile(filepath.Join(root, "proc", entry.Name(), "status"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) || os.IsPermission(err) {
			continue // exited or inaccessible, possibly outside this process tree
		}
		if err != nil {
			return -1
		}
		parent, ok := memoryField(b, "PPid:")
		rss, known := memoryField(b, "VmRSS:")
		if !known {
			// Zombies have no resident pages and omit VmRSS. A present but
			// malformed value still means unknown, not zero.
			known = true
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) > 0 && f[0] == "VmRSS:" {
					known = false
					break
				}
			}
		}
		if ok {
			ps[id] = process{int(parent), rss, known}
		}
	}
	own, ok := ps[pid]
	if !ok || !own.known {
		return -1
	}
	total := own.rss
	seen := map[int]bool{pid: true}
	for more := true; more; {
		more = false
		for id, p := range ps {
			if !seen[id] && seen[p.parent] {
				if !p.known {
					return -1
				}
				seen[id], more = true, true
				total += p.rss
			}
		}
	}
	return total / 1024
}
