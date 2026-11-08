package provider

import (
	"context"
	"strings"
	"sync"
	"time"
)

// RemoteAllowance is a Remote magpie's allowance for one of its models, as
// routing weighs an account's: the windows of the account there that
// serves the model's agent ("claude/claude-opus-5-5" is its Claude
// Code's), from the cards this magpie keeps of it (remoteCards). The
// Usage page showed a remote's windows while routing counted the remote
// as not known, so a group of one Claude per remote magpie went by the
// members' order, never by which renews first. Of several accounts there
// for the agent, the least used stands for the remote, as it is the one
// its own routing has room on. It never waits: cards older than
// remoteCardsFor are read again behind the request.
func RemoteAllowance(p Provider, model string) (Allowance, bool) {
	if !p.IsRemoteMagpie() || p.Key == "" {
		return nil, false
	}
	agent, _, ok := strings.Cut(model, "/")
	if !ok {
		return nil, false
	}
	id := remoteCardsID(p)
	c := &remoteCardCache
	c.Lock()
	r, have := c.m[id]
	c.Unlock()
	if !have || time.Since(r.at) >= remoteCardsFor {
		readRemoteCardsBehind(p, id)
	}
	if !have || r.held {
		return nil, false
	}
	now := time.Now()
	var best Allowance
	bestUsed := 0.0
	for _, q := range r.cards {
		if q.From != "" || q.Provider != agent || q.Error != "" {
			continue
		}
		a := allowanceOf(remoteRoutedWindows(agent, q.Windows), now)
		if len(a) == 0 {
			continue
		}
		if u, _ := a.For(model, now); best == nil || u < bestUsed {
			best, bestUsed = a, u
		}
	}
	return best, best != nil
}

// remoteRoutedWindows puts back on a remote's windows what their JSON
// leaves out and routing reads: how long each runs, from its name, and
// the one model a Claude window scoped as "7 days · Fable" counts. A
// scoped window of another agent is left out, as which models it counts
// isn't known here and it would hold back every model.
func remoteRoutedWindows(agent string, ws []QuotaWindow) []QuotaWindow {
	out := make([]QuotaWindow, 0, len(ws))
	for _, w := range ws {
		if _, scope, scoped := strings.Cut(w.Name, " · "); scoped {
			if agent != "claude" {
				continue
			}
			w.Model = claudeScopeModel(scope)
		}
		w.Span = windowSpan(w)
		out = append(out, w)
	}
	return out
}

var remoteCardsReading sync.Map // remoteCardsID → read under way

func readRemoteCardsBehind(p Provider, id string) {
	if _, busy := remoteCardsReading.LoadOrStore(id, true); busy {
		return
	}
	go func() {
		defer remoteCardsReading.Delete(id)
		ctx, cancel := context.WithTimeout(context.Background(), remoteWait+5*time.Second)
		defer cancel()
		remoteCards(ctx, p, false)
	}()
}
