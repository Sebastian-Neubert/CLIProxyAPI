package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetQuotaHistoryReturnsClaudeAndCodexSeries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	store := quotahistory.NewStore()
	previous := quotaHistoryStore
	quotaHistoryStore = func() *quotahistory.Store { return store }
	t.Cleanup(func() { quotaHistoryStore = previous })

	now := time.Now().UTC().Truncate(time.Second)
	used := 41.0
	reset := now.Add(72 * time.Hour)
	older := 12.0
	store.Record("hist-claude", now.Add(-20*24*time.Hour), []quotahistory.Window{{Name: "7d", WindowMinutes: 10080, UsedPercent: &older, ResetAt: &reset}})
	store.Record("hist-claude", now.Add(-time.Hour), []quotahistory.Window{{Name: "7d", WindowMinutes: 10080, UsedPercent: &used, ResetAt: &reset}})

	manager := coreauth.NewManager(nil, nil, nil)
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "hist-claude", Provider: "claude"})
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "hist-codex", Provider: "codex"})
	registerRuntimeAuth(t, manager, &coreauth.Auth{ID: "hist-gemini", Provider: "gemini"})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/quota/history"+query, nil)
		h.GetQuotaHistory(ginCtx)
		return rec
	}

	rec := get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if got := ojson.GetBytesNoCopy(body, "retention_hours").Int(); got != 672 {
		t.Fatalf("retention_hours = %d", got)
	}
	providers := ojson.GetBytesNoCopy(body, "providers").Array()
	if len(providers) != 2 || providers[0].Get("provider").String() != "claude" || providers[1].Get("provider").String() != "codex" {
		t.Fatalf("providers = %s", ojson.GetBytesNoCopy(body, "providers").Raw)
	}
	points := providers[0].Get("accounts.0.points").Array()
	if len(points) != 1 {
		t.Fatalf("default lookback points = %s, want only the recent point", providers[0].Get("accounts.0.points").Raw)
	}
	window := points[0].Get("windows.0")
	if window.Get("name").String() != "7d" || window.Get("window_minutes").Int() != 10080 || window.Get("used_percent").Float() != 41 {
		t.Fatalf("window = %s", window.Raw)
	}
	if got := providers[1].Get("accounts.0.points").Raw; got != "[]" {
		t.Fatalf("codex points = %s, want []", got)
	}

	since := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if got := ojson.GetBytesNoCopy(get("?since="+since).Body.Bytes(), "providers.0.accounts.0.points").Array(); len(got) != 2 {
		t.Fatalf("since points = %d, want 2", len(got))
	}
	if rec := get("?since=yesterday"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid since status = %d", rec.Code)
	}
}
