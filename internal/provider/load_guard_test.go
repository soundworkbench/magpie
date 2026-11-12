package provider

import (
	"bytes"
	"context"
	"fmt"
	"github.com/yetone/magpie/internal/settings"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadGuardOverloaded(t *testing.T) {
	g := settings.LoadGuardConfig{HostFreeMinMiB: 2048, InstanceMaxMiB: 2560}
	for _, tc := range []struct {
		name       string
		host, self int64
		off        bool
		why        string
	}{
		{"host", 2047, 20, false, "host free 2047 MiB"},
		{"instance", 2048, 2561, false, "instance 2561 MiB"},
		{"boundaries", 2048, 2560, false, ""},
		{"unknown", -1, -1, false, ""},
		{"zero", 0, 0, false, "host free 0 MiB"},
		{"off", 0, 9999, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.Off = tc.off
			over, why := Overloaded(LoadReading{HostAvailableMiB: tc.host, SelfMiB: tc.self}, g)
			if over != (tc.why != "") || why != tc.why {
				t.Fatalf("overloaded = %v %q, want %q", over, why, tc.why)
			}
		})
	}
}

// A held HTTP response proves the caller can route before it arrives.
func TestLoadGuardRemoteCache(t *testing.T) {
	loadGuardHome(t)
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	c := &remoteLoads{m: map[string]remoteLoadRead{}, now: func() time.Time { return time.Unix(clock.Load(), 0) }}
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(200)
	entered, release := make(chan struct{}, 10), make(chan struct{}, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != RemoteLoadPath || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Api-Key") != "test-key" || r.Header.Get("X-Load-Test") != "present" {
			t.Errorf("load request missing path or signing: %s %v", r.URL.Path, r.Header)
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"at":1,"hostAvailableMiB":4096,"selfMiB":12}`)
	}))
	defer server.Close()
	p := Provider{ID: "load-remote", Preset: RemoteMagpiePreset, Key: "test-key", Anthropic: server.URL, Headers: map[string]string{"X-Load-Test": "present"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	get := func() bool { _, ok := c.get(ctx, p); return ok }
	done := make(chan bool, 1)
	go func() { done <- get() }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("first read already known")
		}
	case <-time.After(time.Second):
		t.Fatal("routing waited for HTTP")
	}
	<-entered
	for range 5 {
		if get() {
			t.Fatal("in-flight read already known")
		}
	}
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	if !get() || calls.Load() != 1 {
		t.Fatal("fresh cache was not reused")
	}
	clock.Add(29)
	if !get() || calls.Load() != 1 {
		t.Fatal("cache refreshed before 30 seconds")
	}
	clock.Add(1)
	if !get() {
		t.Fatal("refresh discarded recent reading")
	}
	<-entered
	clock.Add(91)
	if get() {
		t.Fatal("reading older than two minutes is known")
	}
	status.Store(500)
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	// The stale read started 91 seconds ago; start its next attempt now.
	get()
	<-entered
	status.Store(200)
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	if !get() {
		t.Fatal("successful retry did not recover")
	}
	clock.Add(30)
	get()
	<-entered
	status.Store(500)
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	if get() {
		t.Fatal("failed read used a recent successful reading")
	}
	clock.Add(30)
	get()
	<-entered
	status.Store(404)
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	n := calls.Load()
	clock.Add(599)
	if get() || calls.Load() != n {
		t.Fatal("404 retried within ten minutes")
	}
	clock.Add(1)
	get()
	<-entered
	status.Store(200)
	release <- struct{}{}
	loadGuardReadDone(t, c, p)
	if !get() {
		t.Fatal("404 backoff did not expire")
	}
	s := settings.Load()
	s.LoadGuard.Off = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	clock.Add(30)
	if get() || calls.Load() != n+1 {
		t.Fatal("disabled guard read remote")
	}
}

func loadGuardHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := settings.Load()
	s.LoadGuard = settings.LoadGuardConfig{}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

func loadGuardReadDone(t *testing.T, c *remoteLoads, p Provider) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.Lock()
		busy := c.m[remoteCardsID(p)].readingNow
		c.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("remote read did not finish")
}

func TestLoadGuardRemoteTransitions(t *testing.T) {
	loadGuardHome(t)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	var free atomic.Int64
	free.Store(512)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"at":1,"hostAvailableMiB":%d,"selfMiB":100}`, free.Load())
	}))
	defer server.Close()
	p := Provider{ID: "load-remote", Name: "Remote reader", Preset: RemoteMagpiePreset, Key: "key", Anthropic: server.URL}
	c := &remoteLoads{m: map[string]remoteLoadRead{}, now: time.Now}
	for range 2 {
		c.read(context.Background(), p, remoteCardsID(p))
	}
	free.Store(4096)
	for range 2 {
		c.read(context.Background(), p, remoteCardsID(p))
	}
	if got := logs.String(); strings.Count(got, "is overloaded:") != 1 || strings.Count(got, "is no longer overloaded") != 1 || !strings.Contains(got, "Remote reader") || !strings.Contains(got, "host free 512 MiB") {
		t.Fatalf("load transitions: %s", got)
	}
	if _, ok := c.get(context.Background(), Provider{ID: "ordinary", Key: "key"}); ok {
		t.Fatal("ordinary provider has remote load")
	}
}

func TestLoadGuardRemoteMalformed(t *testing.T) {
	loadGuardHome(t)
	for _, body := range []string{`{`, `null`, `{}`, `{"at":1,"selfMiB":"bad"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			if _, _, err := askRemoteLoad(context.Background(), Provider{Anthropic: server.URL, Key: "key"}); err == nil {
				t.Fatal("invalid reading accepted")
			}
		})
	}
}

func TestLoadGuardRemotePollAndStatus(t *testing.T) {
	loadGuardHome(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"at":1,"hostAvailableMiB":100,"selfMiB":50}`)
	}))
	defer server.Close()
	p := Provider{ID: "load-poll", Preset: RemoteMagpiePreset, Key: "key", Anthropic: server.URL}
	now := time.Now()
	c := &remoteLoads{m: map[string]remoteLoadRead{}, now: func() time.Time { return now }}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		keepRemoteLoads(ctx, ticks, c, func() []Provider { return []Provider{p} })
	}()
	defer func() { cancel(); <-stopped }()
	statuses := func() RemoteLoadStatus {
		t.Helper()
		ss := c.statuses([]Provider{p})
		if len(ss) != 1 {
			t.Fatal(ss)
		}
		return ss[0]
	}
	if got := statuses(); got.Known || got.Reading != nil || calls.Load() != 0 {
		t.Fatalf("uncached status fetched: %+v", got)
	}
	ticks <- now
	// The receiver has accepted the tick but may not yet have started get.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	loadGuardReadDone(t, c, p)
	if got := statuses(); !got.Known || !got.Overloaded || got.Reason != "host free 100 MiB" || got.ReadAt != now.Unix() || got.Reading.At != 1 {
		t.Fatalf("poll status: %+v", got)
	}
	c.Lock()
	now = now.Add(121 * time.Second)
	c.Unlock()
	if got := statuses(); got.Known || got.Overloaded || got.Reading == nil {
		t.Fatalf("stale status: %+v", got)
	}
	c.Lock()
	r := c.m[remoteCardsID(p)]
	r.failed = true
	r.missingUntil = now.Add(10 * time.Minute)
	c.m[remoteCardsID(p)] = r
	c.Unlock()
	if got := statuses(); !got.Missing || got.Known {
		t.Fatalf("404 status: %+v", got)
	}
	s := settings.Load()
	s.LoadGuard.Off = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	// A second tick acknowledges that the first off tick has been handled.
	ticks <- now
	ticks <- now
	if calls.Load() != 1 || c.statuses([]Provider{p}) != nil {
		t.Fatal("off guard polled or exposed remote status")
	}
}
