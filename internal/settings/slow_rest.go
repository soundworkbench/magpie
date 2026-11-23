package settings

import "time"

// Defaults for resting a model after consecutive slow starts. They are
// seconds in settings.json, so the settings stay portable and easy to edit.
const (
	SlowForgetDefault   = 30 * 60
	SlowRestsDefault    = 2
	SlowCooldownDefault = 60
)

// SlowRestConfig is the normalized policy the gateway uses for consecutive
// slow starts. Settings store seconds; non-positive values mean default.
type SlowRestConfig struct {
	Forget   time.Duration
	Rests    int
	Cooldown time.Duration
}

// SlowRestNow returns the policy with defaults applied to unset or invalid
// values.
func (s Settings) SlowRestNow() SlowRestConfig {
	forget, rests, cooldown := s.SlowForget, s.SlowRests, s.SlowCooldown
	if forget <= 0 {
		forget = SlowForgetDefault
	}
	if rests <= 0 {
		rests = SlowRestsDefault
	}
	if cooldown <= 0 {
		cooldown = SlowCooldownDefault
	}
	return SlowRestConfig{
		Forget:   seconds(forget),
		Rests:    rests,
		Cooldown: seconds(cooldown),
	}
}

func seconds(n int) time.Duration {
	if int64(n) > int64((1<<63-1)/int64(time.Second)) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(n) * time.Second
}
