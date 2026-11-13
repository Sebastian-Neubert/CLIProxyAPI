package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaSignals(fiveHourReset, weeklyReset time.Time) map[string]string {
	return map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.12",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(fiveHourReset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.53",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weeklyReset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-Status":         "allowed",
	}
}

func headersFromSignals(signals map[string]string) http.Header {
	headers := make(http.Header, len(signals))
	for key, value := range signals {
		headers.Set(key, value)
	}
	return headers
}

func TestQuotaWindowsForAuthParsesClaudeAndCodexSnapshots(t *testing.T) {
	observedAt := time.Unix(1787279282, 0)
	fiveHour := observedAt.Add(3 * time.Hour)
	weekly := observedAt.Add(4 * 24 * time.Hour)

	claude := &Auth{ID: "claude", Provider: "claude", Quota: QuotaState{ObservedAt: observedAt, Signals: claudeQuotaSignals(fiveHour, weekly)}}
	windows := QuotaWindowsForAuth(claude)
	if len(windows) != 2 || windows[0].Name != "5h" || windows[1].Name != "7d" {
		t.Fatalf("Claude windows = %#v, want 5h and 7d", windows)
	}
	if windows[1].Duration != 7*24*time.Hour || !windows[1].ResetAt.Equal(weekly) || windows[1].Status != "allowed" {
		t.Fatalf("Claude weekly window = %#v", windows[1])
	}
	if windows[1].UsedPercent == nil || *windows[1].UsedPercent != 53 {
		t.Fatalf("Claude weekly used percent = %v, want 53", windows[1].UsedPercent)
	}

	codex := &Auth{ID: "codex", Provider: "codex", Quota: QuotaState{ObservedAt: observedAt, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":          "51",
		"X-Codex-Primary-Window-Minutes":        "300",
		"X-Codex-Primary-Reset-At":              strconv.FormatInt(fiveHour.Unix(), 10),
		"X-Codex-Secondary-Used-Percent":        "20",
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-After-Seconds": "345600",
		"X-Codex-Bengalfox-Secondary-Reset-At":  "1",
	}}}
	windows = QuotaWindowsForAuth(codex)
	if len(windows) != 2 || windows[0].Name != "primary" || windows[1].Name != "secondary" {
		t.Fatalf("Codex windows = %#v, want primary and secondary", windows)
	}
	if !windows[0].ResetAt.Equal(fiveHour) {
		t.Fatalf("Codex primary reset = %v, want %v", windows[0].ResetAt, fiveHour)
	}
	if !windows[1].ResetAt.Equal(weekly) || windows[1].Duration != 7*24*time.Hour {
		t.Fatalf("Codex secondary window = %#v, want reset-after-seconds relative to observation", windows[1])
	}

	reset, ok := LongWindowQuotaReset(codex, observedAt)
	if !ok || reset.Name != "secondary" || !reset.ResetAt.Equal(weekly) {
		t.Fatalf("Codex long window = %#v ok=%v, want secondary weekly reset", reset, ok)
	}
}

func TestLongWindowQuotaResetIgnoresShortPastAndUnsupportedWindows(t *testing.T) {
	now := time.Unix(1787279282, 0)
	for _, testCase := range []struct {
		name string
		auth *Auth
	}{
		{name: "no snapshot", auth: &Auth{ID: "a", Provider: "claude"}},
		{name: "five hour only", auth: &Auth{ID: "b", Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		}}}},
		{name: "weekly reset already passed", auth: &Auth{ID: "c", Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: claudeQuotaSignals(now.Add(-2*time.Hour), now.Add(-time.Minute))}}},
		{name: "codex window length unknown", auth: &Auth{ID: "d", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Reset-At": strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
		}}}},
		{name: "unsupported provider", auth: &Auth{ID: "e", Provider: "gemini", Quota: QuotaState{ObservedAt: now, Signals: claudeQuotaSignals(now.Add(time.Hour), now.Add(48*time.Hour))}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if window, ok := LongWindowQuotaReset(testCase.auth, now); ok {
				t.Fatalf("LongWindowQuotaReset() = %#v, want unknown", window)
			}
		})
	}
}

func TestSoonestResetSelectorPrefersEarliestKnownResetThenRotatesUnknown(t *testing.T) {
	now := time.Now()
	withReset := func(id string, weekly time.Time) *Auth {
		return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: claudeQuotaSignals(now.Add(time.Hour), weekly)}}
	}
	selector := NewSoonestResetSelector()
	selector.now = func() time.Time { return now }
	ctx := context.Background()
	pick := func(auths ...*Auth) string {
		t.Helper()
		got, err := selector.Pick(ctx, "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return got.ID
	}

	late := withReset("a-late", now.Add(6*24*time.Hour))
	soon := withReset("z-soon", now.Add(24*time.Hour))
	unknown := &Auth{ID: "0-unknown", Provider: "claude", Status: StatusActive}
	for range 3 {
		if got := pick(late, unknown, soon); got != "z-soon" {
			t.Fatalf("Pick() = %q, want earliest weekly reset z-soon", got)
		}
	}

	cooling := withReset("cooling", now.Add(time.Hour*25))
	soonCooling := withReset("y-soon-cooldown", now.Add(2*time.Hour))
	soonCooling.Unavailable = true
	soonCooling.Quota.Exceeded = true
	soonCooling.NextRetryAfter = now.Add(time.Hour)
	if got := pick(late, soonCooling, cooling); got != "cooling" {
		t.Fatalf("Pick() = %q, want cooldown credential skipped", got)
	}

	disabled := withReset("x-disabled", now.Add(2*time.Hour))
	disabled.Disabled = true
	disabled.Status = StatusDisabled
	if got := pick(late, disabled); got != "a-late" {
		t.Fatalf("Pick() = %q, want disabled credential skipped", got)
	}

	lowPriority := withReset("w-low-priority", now.Add(2*time.Hour))
	lowPriority.Attributes = map[string]string{"priority": "-1"}
	if got := pick(late, lowPriority); got != "a-late" {
		t.Fatalf("Pick() = %q, want highest priority tier before reset ranking", got)
	}

	first := &Auth{ID: "u1", Provider: "claude", Status: StatusActive}
	second := &Auth{ID: "u2", Provider: "claude", Status: StatusActive}
	seen := map[string]int{}
	for range 4 {
		seen[pick(first, second)]++
	}
	if seen["u1"] != 2 || seen["u2"] != 2 {
		t.Fatalf("unknown credentials picks = %v, want round-robin", seen)
	}
}

func TestRankAuthsBySoonestResetOrdersKnownBeforeUnknown(t *testing.T) {
	now := time.Unix(1787279282, 0)
	auths := []*Auth{
		{ID: "unknown-b", Provider: "codex"},
		{ID: "late", Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: claudeQuotaSignals(now.Add(time.Hour), now.Add(5*24*time.Hour))}},
		{ID: "unknown-a", Provider: "claude"},
		{ID: "soon", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes": "10080",
			"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(30*time.Hour).Unix(), 10),
		}}},
	}
	ranked := RankAuthsBySoonestReset(auths, now)
	var order []string
	for _, candidate := range ranked {
		order = append(order, candidate.Auth.ID)
	}
	want := []string{"soon", "late", "unknown-a", "unknown-b"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ranking = %v, want %v", order, want)
		}
	}
	if ranked[0].Window != "primary" || ranked[1].Window != "7d" {
		t.Fatalf("ranked windows = %q/%q, want primary/7d", ranked[0].Window, ranked[1].Window)
	}
}

// TestManagerSoonestResetWithSessionAffinityKeepsThreadsBound covers the full
// path: reset times are learned from real response headers through MarkResult,
// new threads follow the soonest reset, an established thread keeps its
// credential across prompts and tool turns even when the ranking changes, and
// it only moves once its credential enters cooldown.
func TestManagerSoonestResetWithSessionAffinityKeepsThreadsBound(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		provider      string
		sessionHeader string
		weeklyHeaders func(reset time.Time) http.Header
	}{
		{
			name:          "claude code",
			provider:      "claude",
			sessionHeader: "X-Claude-Code-Session-Id",
			weeklyHeaders: func(reset time.Time) http.Header {
				return headersFromSignals(claudeQuotaSignals(time.Now().Add(2*time.Hour), reset))
			},
		},
		{
			name:          "codex",
			provider:      "codex",
			sessionHeader: "Session_id",
			weeklyHeaders: func(reset time.Time) http.Header {
				return http.Header{
					"X-Codex-Primary-Used-Percent":          {"10"},
					"X-Codex-Primary-Window-Minutes":        {"300"},
					"X-Codex-Primary-Reset-After-Seconds":   {"3600"},
					"X-Codex-Secondary-Used-Percent":        {"40"},
					"X-Codex-Secondary-Window-Minutes":      {"10080"},
					"X-Codex-Secondary-Reset-After-Seconds": {strconv.FormatInt(int64(time.Until(reset).Seconds()), 10)},
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			provider := testCase.provider
			model := "soonest-reset-model-" + provider
			idA, idB, idC := "soonest-"+provider+"-a", "soonest-"+provider+"-b", "soonest-"+provider+"-c"

			manager := NewManager(nil, nil, nil)
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback: NewSoonestResetSelector(),
				TTL:      time.Hour,
				RebindOn: SessionAffinityRebindOnQuota,
			})
			defer affinity.Stop()
			manager.SetSelector(affinity)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, id := range []string{idA, idB, idC} {
				if _, errRegister := manager.Register(WithSkipPersist(ctx), &Auth{ID: id, Provider: provider, Status: StatusActive}); errRegister != nil {
					t.Fatalf("Register(%s): %v", id, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				authID := id
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
			}

			observe := func(authID string, headers http.Header, failure *Error) {
				t.Helper()
				resultCtx := internallogging.WithResponseHeadersHolder(ctx)
				internallogging.SetResponseHeaders(resultCtx, headers)
				manager.MarkResult(resultCtx, Result{AuthID: authID, Provider: provider, Model: model, Success: failure == nil, Error: failure})
			}
			sessionOpts := func(session, body string) cliproxyexecutor.Options {
				return cliproxyexecutor.Options{
					Headers:         http.Header{testCase.sessionHeader: {session}},
					OriginalRequest: []byte(body),
					Metadata:        map[string]any{},
				}
			}
			pick := func(opts cliproxyexecutor.Options) string {
				t.Helper()
				auth, _, errPick := manager.pickNext(ctx, provider, model, opts, nil)
				if errPick != nil {
					t.Fatalf("pickNext: %v", errPick)
				}
				return auth.ID
			}

			now := time.Now()
			observe(idA, testCase.weeklyHeaders(now.Add(5*24*time.Hour)), nil)
			observe(idB, testCase.weeklyHeaders(now.Add(2*24*time.Hour)), nil)

			// A new thread binds to the credential whose weekly quota resets first.
			turns := []string{
				`{"messages":[{"role":"user","content":"refactor the parser"}]}`,
				`{"messages":[{"role":"user","content":"refactor the parser"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"..."}]}]}`,
				`{"messages":[{"role":"user","content":"now add tests"}]}`,
			}
			for i, body := range turns {
				got := pick(sessionOpts("thread-1", body))
				if got != idB {
					t.Fatalf("thread-1 turn %d picked %q, want soonest reset %q", i, got, idB)
				}
				observe(got, testCase.weeklyHeaders(now.Add(2*24*time.Hour)), nil)
			}

			// The ranking changes: C now resets sooner than B. New threads follow
			// the ranking, the established thread keeps its credential.
			observe(idC, testCase.weeklyHeaders(now.Add(26*time.Hour)), nil)
			if got := pick(sessionOpts("thread-2", turns[0])); got != idC {
				t.Fatalf("thread-2 picked %q, want new soonest reset %q", got, idC)
			}
			for i, body := range turns {
				if got := pick(sessionOpts("thread-1", body)); got != idB {
					t.Fatalf("thread-1 turn %d after ranking change picked %q, want bound %q", i, got, idB)
				}
			}

			// A transient upstream failure blocks the credential briefly. With
			// rebind-on "quota" that request fails over, but the binding stays and
			// the thread returns as soon as the transient block ends.
			observe(idB, nil, &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream reset"})
			if got := pick(sessionOpts("thread-1", turns[2])); got != idC {
				t.Fatalf("thread-1 during transient block picked %q, want failover to soonest available %q", got, idC)
			}
			expireSessionAffinityPriorityModelCooldown(t, manager, idB, model)
			if got := pick(sessionOpts("thread-1", turns[2])); got != idB {
				t.Fatalf("thread-1 after transient 502 picked %q, want bound %q", got, idB)
			}

			// Once the bound credential is rate limited the thread moves to the
			// soonest-reset credential that is still available.
			observe(idB, nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"})
			if got := pick(sessionOpts("thread-1", turns[2])); got != idC {
				t.Fatalf("thread-1 after 429 picked %q, want soonest available %q", got, idC)
			}
			if got := pick(sessionOpts("thread-1", turns[0])); got != idC {
				t.Fatalf("thread-1 did not stay on its new binding: got %q, want %q", got, idC)
			}
		})
	}
}
