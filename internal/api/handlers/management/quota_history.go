package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const defaultQuotaHistoryLookback = 14 * 24 * time.Hour

// quotaHistoryStore is the history source; tests may replace it.
var quotaHistoryStore = quotahistory.Default

// GetQuotaHistory returns recorded passive quota window observations per
// Claude/Codex credential for the pooled subscription usage chart. It is
// read-only and does not touch the usage queue.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	now := time.Now()
	since := now.Add(-defaultQuotaHistoryLookback)
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be an RFC3339 timestamp"})
			return
		}
		since = parsed
	}
	store := quotaHistoryStore()

	byProvider := make(map[string][]*coreauth.Auth)
	for _, auth := range h.authManager.List() {
		if auth == nil || !isAuthFileListable(auth) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider != "claude" && provider != "codex" {
			continue
		}
		byProvider[provider] = append(byProvider[provider], auth)
	}
	providerNames := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providerNames = append(providerNames, provider)
	}
	sort.Strings(providerNames)

	providers := make([]gin.H, 0, len(providerNames))
	for _, provider := range providerNames {
		auths := byProvider[provider]
		sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
		accounts := make([]gin.H, 0, len(auths))
		for _, auth := range auths {
			points := store.Series(auth.ID, since)
			item := gin.H{
				"id":         auth.ID,
				"auth_index": auth.Index,
				"name":       authDisplayName(auth),
				"disabled":   auth.Disabled,
				"points":     points,
			}
			if email := authEmail(auth); email != "" {
				item["email"] = email
			}
			accounts = append(accounts, item)
		}
		providers = append(providers, gin.H{"provider": provider, "accounts": accounts})
	}

	c.JSON(http.StatusOK, gin.H{
		"generated_at":    now.UTC(),
		"since":           since.UTC(),
		"retention_hours": int64(store.Retention() / time.Hour),
		"providers":       providers,
	})
}
