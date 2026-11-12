package settings

// LoadGuardConfig bounds new conversations and idle processes on this machine.
// Nonpositive numbers use the defaults; Off restores the previous behavior.
type LoadGuardConfig struct {
	Off                bool  `json:"off,omitempty"`
	HostFreeMinMiB     int64 `json:"hostFreeMinMiB,omitempty"`
	InstanceMaxMiB     int64 `json:"instanceMaxMiB,omitempty"`
	IdleProcessMinutes int   `json:"idleProcessMinutes,omitempty"`
}

// LoadGuardNow reads this machine's current load guard settings.
func LoadGuardNow() LoadGuardConfig {
	g := Shared().LoadGuard
	if g.HostFreeMinMiB <= 0 {
		g.HostFreeMinMiB = 2048
	}
	if g.InstanceMaxMiB <= 0 {
		g.InstanceMaxMiB = 2560
	}
	if g.IdleProcessMinutes <= 0 {
		g.IdleProcessMinutes = 30
	}
	return g
}
