package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// RemoteLoadPath is the gateway's read-only load snapshot.
const RemoteLoadPath = "/v1/magpie/load"

// LoadReading describes the host and this instance, in MiB. Negative memory
// readings are unknown; a zero SelfLimitMiB means no cgroup limit.
type LoadReading struct {
	At               int64 `json:"at"`
	HostAvailableMiB int64 `json:"hostAvailableMiB"`
	HostTotalMiB     int64 `json:"hostTotalMiB"`
	SelfMiB          int64 `json:"selfMiB"`
	SelfLimitMiB     int64 `json:"selfLimitMiB"`
	ClaudeRuns       int   `json:"claudeRuns"`
	ClaudeIdle       int   `json:"claudeIdle"`
}

const remoteLoadFor = 30 * time.Second
const remoteLoadFresh = 2 * time.Minute
const remoteLoadMissingFor = 10 * time.Minute

type remoteLoadRead struct {
	reading                    LoadReading
	at, tried, missingUntil    time.Time
	readingNow, failed, loaded bool
}

type remoteLoads struct {
	sync.Mutex
	m   map[string]remoteLoadRead
	now func() time.Time
}

var loadCache = remoteLoads{m: make(map[string]remoteLoadRead), now: time.Now}

// RemoteLoad returns a recent remote reading without waiting for a fetch.
// Failed and stale readings are unknown, never evidence of overload.
func RemoteLoad(p Provider) (LoadReading, bool) {
	return loadCache.get(context.Background(), p)
}

func (c *remoteLoads) get(ctx context.Context, p Provider) (LoadReading, bool) {
	g := settings.LoadGuardNow()
	if g.Off || !p.IsRemoteMagpie() || p.Key == "" {
		return LoadReading{}, false
	}
	id := remoteCardsID(p)
	c.Lock()
	now := c.now()
	r := c.m[id]
	if !r.readingNow && !now.Before(r.missingUntil) && (r.tried.IsZero() || now.Sub(r.tried) >= remoteLoadFor) {
		r.readingNow, r.tried = true, now
		c.m[id] = r
		go c.read(ctx, p, id)
	}
	c.Unlock()
	return r.reading, !r.failed && !r.at.IsZero() && now.Sub(r.at) <= remoteLoadFresh
}

func (c *remoteLoads) read(ctx context.Context, p Provider, id string) {
	reading, missing, err := askRemoteLoad(ctx, p)
	g := settings.LoadGuardNow()
	c.Lock()
	defer c.Unlock()
	now := c.now()
	r := c.m[id]
	r.readingNow, r.failed = false, err != nil
	if missing {
		r.missingUntil = now.Add(remoteLoadMissingFor)
	}
	if err == nil {
		r.reading, r.at = reading, now
		loaded, why := Overloaded(reading, g)
		if !g.Off && loaded != r.loaded {
			if loaded {
				log.Printf("provider: %s is overloaded: %s (host free %d MiB, instance %d MiB)", remoteName(p), why, reading.HostAvailableMiB, reading.SelfMiB)
			} else {
				log.Printf("provider: %s is no longer overloaded (host free %d MiB, instance %d MiB)", remoteName(p), reading.HostAvailableMiB, reading.SelfMiB)
			}
			r.loaded = loaded
		}
	}
	c.m[id] = r
}

// askRemoteLoad uses the same signing, proxy and timeout as quota reads,
// but reads a single object and distinguishes an older gateway's 404.
func askRemoteLoad(ctx context.Context, p Provider) (LoadReading, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Anthropic+RemoteLoadPath, nil)
	if err != nil {
		return LoadReading{}, false, err
	}
	req.Header.Set("Accept", "application/json")
	p.signRemote(req, p.Key)
	res, err := p.Do(http.DefaultClient, req)
	if err != nil {
		return LoadReading{}, false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return LoadReading{}, res.StatusCode == http.StatusNotFound, fmt.Errorf("remote load: %s", res.Status)
	}
	// Missing fields must stay unknown, not become a host with no free memory.
	r := LoadReading{HostAvailableMiB: -1, HostTotalMiB: -1, SelfMiB: -1, SelfLimitMiB: -1}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err == nil && r.At <= 0 {
		err = fmt.Errorf("remote load has no reading time")
	}
	return r, false, err
}

// KeepRemoteLoads refreshes configured remotes even when no requests arrive.
// Its reads end with the gateway's context; disabling the guard starts none.
func KeepRemoteLoads(ctx context.Context) {
	tick := time.NewTicker(remoteLoadFor)
	defer tick.Stop()
	keepRemoteLoads(ctx, tick.C, &loadCache, remoteMagpies)
}

func keepRemoteLoads(ctx context.Context, ticks <-chan time.Time, c *remoteLoads, remotes func() []Provider) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if !settings.LoadGuardNow().Off {
				for _, p := range remotes() {
					c.get(ctx, p)
				}
			}
		}
	}
}

// RemoteLoadStatus includes the last successful reading even when it is
// no longer usable for routing. ReadAt is local reception time, not At.
type RemoteLoadStatus struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Reading    *LoadReading `json:"reading,omitempty"`
	ReadAt     int64        `json:"readAt,omitempty"`
	Known      bool         `json:"known"`
	Overloaded bool         `json:"overloaded"`
	Reason     string       `json:"reason,omitempty"`
	Missing    bool         `json:"missing"`
}

// RemoteLoadStatuses only reads the cache: a status request never fetches
// down a chain of gateways, including gateways that point at each other.
func RemoteLoadStatuses() []RemoteLoadStatus {
	return loadCache.statuses(remoteMagpies())
}

func (c *remoteLoads) statuses(ps []Provider) []RemoteLoadStatus {
	g := settings.LoadGuardNow()
	if g.Off {
		return nil
	}
	var out []RemoteLoadStatus
	for _, p := range ps {
		c.Lock()
		r, now := c.m[remoteCardsID(p)], c.now()
		c.Unlock()
		s := RemoteLoadStatus{ID: p.ID, Name: remoteName(p), Missing: now.Before(r.missingUntil)}
		if !r.at.IsZero() {
			s.Reading, s.ReadAt = &r.reading, r.at.Unix()
			s.Known = !r.failed && now.Sub(r.at) <= remoteLoadFresh
			if s.Known {
				s.Overloaded, s.Reason = Overloaded(r.reading, g)
			}
		}
		out = append(out, s)
	}
	return out
}

// Overloaded says whether a known memory reading exceeds the guard.
func Overloaded(r LoadReading, g settings.LoadGuardConfig) (bool, string) {
	if g.Off {
		return false, ""
	}
	if r.HostAvailableMiB >= 0 && r.HostAvailableMiB < g.HostFreeMinMiB {
		return true, fmt.Sprintf("host free %d MiB", r.HostAvailableMiB)
	}
	if r.SelfMiB >= 0 && r.SelfMiB > g.InstanceMaxMiB {
		return true, fmt.Sprintf("instance %d MiB", r.SelfMiB)
	}
	return false, ""
}
