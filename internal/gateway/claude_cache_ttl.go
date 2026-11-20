package gateway

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/settings"
)

type claudeCacheTTLKey struct{}

type claudeCacheTTLRequest struct {
	once    sync.Once
	counted *atomic.Int64
}

// withClaudeCacheTTL judges the model before routing rewrites it to a
// member's upstream name. Only passOnCaller consumes this policy: a
// direct vendor never sees magpie's cache lifetime header.
func (s *Server) withClaudeCacheTTL(ctx context.Context, model string) context.Context {
	c := settings.Shared().ClaudeCacheTTL
	id, name := callerKeyOf(ctx)
	keyMatches := func(keys []string) bool {
		for _, key := range keys {
			if key != "" && (key == id || key == name) {
				return true
			}
		}
		return false
	}
	if c.TTL != "1h" || keyMatches(c.ExceptKeys) || len(c.Keys) > 0 && !keyMatches(c.Keys) || !access.ModelAllowed(c.Groups, model) {
		return ctx
	}
	return context.WithValue(ctx, claudeCacheTTLKey{}, &claudeCacheTTLRequest{counted: &s.claudeCacheForced})
}

func remoteClaudeTTL(ctx context.Context, c caller) string {
	ttl := claudeTTLOf(c)
	if ttl != "" && ttl != "default" {
		return ttl
	}
	if policy, ok := ctx.Value(claudeCacheTTLKey{}).(*claudeCacheTTLRequest); ok {
		// Retries and fallback hops still belong to one caller request.
		policy.once.Do(func() { policy.counted.Add(1) })
		return "1h"
	}
	return ttl
}
