package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func registerRuntimeAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	auth.Attributes = map[string]string{"runtime_only": "true"}
	auth.Metadata = map[string]any{"type": auth.Provider}
	auth.Status = coreauth.StatusActive
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register(%s): %v", auth.ID, errRegister)
	}
}

func claudeWeeklySignals(observed time.Time, weeklyReset time.Time) map[string]string {
	return map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(observed.Add(2*time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weeklyReset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.6",
	}
}

func TestGetRoutingPreferenceRanksBySoonestReset(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	now := time.Now()
	manager := coreauth.NewManager(nil, nil, nil)
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "pref-claude-late", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: claudeWeeklySignals(now, now.Add(5*24*time.Hour))}})
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "pref-claude-soon", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: claudeWeeklySignals(now, now.Add(30*time.Hour))}})
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "pref-claude-unknown", Provider: "claude"})
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "pref-gemini", Provider: "gemini"})

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		Routing: config.RoutingConfig{Strategy: "sr", SessionAffinity: true, SessionAffinityRebindOn: "quota"},
	}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/preference", nil)
	h.GetRoutingPreference(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if got := ojson.GetBytesNoCopy(body, "strategy").String(); got != "soonest-reset" {
		t.Fatalf("strategy = %q", got)
	}
	if !ojson.GetBytesNoCopy(body, "strategy_active").Bool() || !ojson.GetBytesNoCopy(body, "session_affinity").Bool() ||
		ojson.GetBytesNoCopy(body, "session_affinity_rebind_on").String() != "quota" {
		t.Fatalf("routing flags = %s", body)
	}
	providers := ojson.GetBytesNoCopy(body, "providers").Array()
	if len(providers) != 1 || providers[0].Get("provider").String() != "claude" {
		t.Fatalf("providers = %s, want only claude", ojson.GetBytesNoCopy(body, "providers").Raw)
	}
	claude := providers[0]
	if got := claude.Get("preferred_id").String(); got != "pref-claude-soon" {
		t.Fatalf("preferred_id = %q, want pref-claude-soon", got)
	}
	candidates := claude.Get("candidates").Array()
	wantOrder := []string{"pref-claude-soon", "pref-claude-late", "pref-claude-unknown"}
	if len(candidates) != len(wantOrder) {
		t.Fatalf("candidates = %s", claude.Get("candidates").Raw)
	}
	for i, want := range wantOrder {
		if got := candidates[i].Get("id").String(); got != want {
			t.Fatalf("candidate %d = %q, want %q", i, got, want)
		}
	}
	if !candidates[0].Get("preferred").Bool() || candidates[1].Get("preferred").Bool() {
		t.Fatalf("preferred flags wrong: %s", claude.Get("candidates").Raw)
	}
	if candidates[0].Get("window").String() != "7d" || candidates[0].Get("reset_at").String() == "" {
		t.Fatalf("soonest candidate reset missing: %s", candidates[0].Raw)
	}
	if candidates[2].Get("reset_at").Exists() {
		t.Fatalf("unknown candidate reported a reset: %s", candidates[2].Raw)
	}
}

func TestListAuthFilesIncludesQuotaWindowsResetAndTokenUsage(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	now := time.Now()
	manager := coreauth.NewManager(nil, nil, nil)
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "entry-claude", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: claudeWeeklySignals(now, now.Add(48*time.Hour))}})
	authusage.Default().HandleUsage(context.Background(), coreusage.Record{AuthID: "entry-claude", Provider: "claude", RequestedAt: now, Detail: coreusage.Detail{
		InputTokens: 10, CacheReadTokens: 900, CacheCreationTokens: 90, OutputTokens: 5,
	}})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	h.ListAuthFiles(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	entry := ojson.GetBytesNoCopy(rec.Body.Bytes(), "files.0")
	windows := entry.Get("quota_windows").Array()
	if len(windows) != 2 || windows[1].Get("name").String() != "7d" || windows[1].Get("window_minutes").Int() != 10080 ||
		windows[1].Get("used_percent").Float() != 60 || windows[1].Get("status").String() != "allowed" {
		t.Fatalf("quota_windows = %s", entry.Get("quota_windows").Raw)
	}
	if entry.Get("quota_reset.window").String() != "7d" || entry.Get("quota_reset.reset_at").String() == "" {
		t.Fatalf("quota_reset = %s", entry.Get("quota_reset").Raw)
	}
	usage := entry.Get("token_usage.current_hour")
	if usage.Get("requests").Int() != 1 || usage.Get("cache_read_tokens").Int() != 900 || usage.Get("cache_write_tokens").Int() != 90 || usage.Get("input_tokens").Int() != 10 {
		t.Fatalf("token_usage = %s", entry.Get("token_usage").Raw)
	}
}
