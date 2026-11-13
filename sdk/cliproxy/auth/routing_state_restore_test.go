package auth

import (
	"context"
	"testing"
	"time"
)

func TestSessionCacheSnapshotRestoreRoundTrip(t *testing.T) {
	source := NewSessionCache(time.Hour)
	defer source.Stop()
	source.setSessionAliases("auth-a", "claude", "session-1", "session-1", "pck:abc")
	source.setSessionAliases("auth-b", "codex", "session-2", "session-2")
	now := time.Now()

	records := source.Snapshot(now)
	if len(records) != 2 {
		t.Fatalf("snapshot = %+v, want 2 groups", records)
	}

	target := NewSessionCache(time.Hour)
	defer target.Stop()
	target.setSessionAliases("auth-live", "claude", "session-2", "session-2")
	expired := SessionBindingRecord{AuthID: "auth-x", ExpiresAt: now.Add(-time.Minute), Aliases: []string{"session-3"}}

	restored := target.Restore(append(records, expired), now)
	if restored != 1 {
		t.Fatalf("restored = %d, want 1 (live binding and expired record skipped)", restored)
	}
	if got, ok := target.Get("pck:abc"); !ok || got != "auth-a" {
		t.Fatalf("alias binding = %q/%v, want auth-a", got, ok)
	}
	if got, _ := target.Get("session-2"); got != "auth-live" {
		t.Fatalf("live binding overwritten: %q", got)
	}
	if _, ok := target.Get("session-3"); ok {
		t.Fatal("expired record restored")
	}
	if got := target.latestSessionAuth("session-1", map[string]struct{}{"claude": {}}); got != "auth-a" {
		t.Fatalf("latestSessionAuth = %q, want auth-a (binding metadata restored)", got)
	}
	// New selections must order after restored ones.
	target.setSessionAliases("auth-c", "claude", "session-1", "session-1b")
	if got := target.latestSessionAuth("session-1", map[string]struct{}{"claude": {}}); got != "auth-c" {
		t.Fatalf("latestSessionAuth after new binding = %q, want auth-c", got)
	}
}

func TestRestoreQuotaObservationOnlyWhenNewer(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	live := &Auth{ID: "q-claude", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"runtime_only": "true"},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5"}}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), live); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}

	older := QuotaObservation{Provider: "claude", ObservedAt: now.Add(-time.Hour), Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.1"}}
	if applied, registered := manager.RestoreQuotaObservation("q-claude", older); applied || !registered {
		t.Fatalf("older snapshot applied=%v registered=%v, want false/true", applied, registered)
	}
	if applied, registered := manager.RestoreQuotaObservation("q-missing", older); applied || registered {
		t.Fatalf("missing auth applied=%v registered=%v, want false/false", applied, registered)
	}
	wrongProvider := QuotaObservation{Provider: "codex", ObservedAt: now.Add(time.Minute), Signals: map[string]string{"x": "1"}}
	if applied, _ := manager.RestoreQuotaObservation("q-claude", wrongProvider); applied {
		t.Fatal("snapshot of another provider applied")
	}
	newer := QuotaObservation{Provider: "claude", ObservedAt: now.Add(time.Minute), Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.7"}}
	if applied, _ := manager.RestoreQuotaObservation("q-claude", newer); !applied {
		t.Fatal("newer snapshot not applied")
	}
	got, _ := manager.GetByID("q-claude")
	if got.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.7" || !got.Quota.ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("quota = %+v, want restored snapshot", got.Quota)
	}
	if observations := manager.QuotaObservations(); observations["q-claude"].Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.7" {
		t.Fatalf("QuotaObservations = %+v", observations)
	}
}

func TestPreviewSoonestResetHonorsRequestedModelCooldown(t *testing.T) {
	now := time.Now()
	cooled := &Auth{ID: "m-a", Provider: "claude", Status: StatusActive, ModelStates: map[string]*ModelState{
		"claude-opus-5-5": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour)}},
	}}
	other := &Auth{ID: "m-b", Provider: "claude", Status: StatusActive}

	agnostic, _ := PreviewSoonestReset([]*Auth{cooled, other}, "claude", "", now)
	if !agnostic[0].Available || !agnostic[1].Available {
		t.Fatalf("model-agnostic preview = %+v, want both available", agnostic)
	}
	if len(agnostic[0].ModelCooldowns) != 1 || agnostic[0].ModelCooldowns[0].Model != "claude-opus-5-5" || !agnostic[0].ModelCooldowns[0].Quota {
		t.Fatalf("model cooldowns = %+v", agnostic[0].ModelCooldowns)
	}

	forModel, _ := PreviewSoonestReset([]*Auth{cooled, other}, "claude", "claude-opus-5-5", now)
	if forModel[0].Auth.ID != "m-a" || forModel[0].Available {
		t.Fatalf("preview for cooled model = %+v, want m-a unavailable", forModel[0])
	}
	if !forModel[1].Available {
		t.Fatalf("preview for cooled model = %+v, want m-b available", forModel[1])
	}
}
