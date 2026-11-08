package provider

import (
	"testing"
	"time"
)

func TestRemoteAllowanceIsItsClaudeAccountsWindows(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	now := time.Now()
	week, hours := now.Add(60*time.Hour), now.Add(2*time.Hour)
	p := Provider{ID: "claude-c", Preset: RemoteMagpiePreset, Key: "sk-magpie-test"}
	keepRemoteCards(remoteCardsID(p), now, []SubscriptionQuota{
		{Provider: "codex", Windows: []QuotaWindow{{Name: "7 days", Used: 99, ResetsAt: &week}}},
		{Provider: "claude", Windows: []QuotaWindow{
			{Name: "5 hours", Used: 9, ResetsAt: &hours},
			{Name: "7 days", Used: 65, ResetsAt: &week},
			{Name: "7 days · Fable", Used: 100, ResetsAt: &week},
		}},
	}, nil)

	a, ok := RemoteAllowance(p, "claude/claude-opus-5-5")
	if !ok {
		t.Fatal("a remote whose Claude card is kept is not known to routing")
	}
	if used, _ := a.For("claude/claude-opus-5-5", now); used != 65 {
		t.Fatalf("Opus is counted at %v%% used, want the week's 65%% (not Codex's, not Fable's)", used)
	}
	if used, _ := a.For("claude/claude-fable-5-1", now); used != 100 {
		t.Fatalf("Fable is counted at %v%% used, want its own window's 100%%", used)
	}
	if got := a.Renewal("claude/claude-opus-5-5", now); len(got) == 0 || !got[0].Equal(week) {
		t.Fatalf("renews %v, want the week's reset %v first", got, week)
	}
}

func TestRemoteAllowanceTakesTheLeastUsedAccount(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	now := time.Now()
	week := now.Add(100 * time.Hour)
	p := Provider{ID: "pool", Preset: RemoteMagpiePreset, Key: "sk-magpie-test"}
	keepRemoteCards(remoteCardsID(p), now, []SubscriptionQuota{
		{Provider: "claude", User: "a", Windows: []QuotaWindow{{Name: "7 days", Used: 80, ResetsAt: &week}}},
		{Provider: "claude", User: "b", Windows: []QuotaWindow{{Name: "7 days", Used: 30, ResetsAt: &week}}},
	}, nil)
	a, ok := RemoteAllowance(p, "claude/claude-sonnet-5-5")
	if !ok {
		t.Fatal("not known")
	}
	if used, _ := a.For("claude/claude-sonnet-5-5", now); used != 30 {
		t.Fatalf("used %v%%, want the account with room's 30%%", used)
	}
}

func TestRemoteAllowanceUnknownWithoutCards(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	if _, ok := RemoteAllowance(Provider{ID: "k", Key: "sk-x"}, "claude/claude-opus-5-5"); ok {
		t.Fatal("a key that is no Remote magpie has a remote allowance")
	}
}
