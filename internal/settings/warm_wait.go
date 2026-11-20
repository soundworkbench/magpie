package settings

// WarmWaitConfig lets a hot conversation wait briefly for its answerer's
// concurrency slot before giving up the cache by moving elsewhere.
type WarmWaitConfig struct {
	Off     bool `json:"off,omitempty"`
	Seconds int  `json:"seconds,omitempty"`
}

// WarmWaitNow reads the current policy; zero uses 30 seconds, and an
// explicit duration is bounded so a warm cache cannot hold a turn forever.
func WarmWaitNow() WarmWaitConfig {
	w := Shared().WarmWait
	if w.Seconds == 0 {
		w.Seconds = 30
	}
	w.Seconds = min(60, max(20, w.Seconds))
	return w
}
