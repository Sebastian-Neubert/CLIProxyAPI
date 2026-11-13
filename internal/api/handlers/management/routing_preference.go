package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// quotaWindowsPayload renders the parsed quota windows of a credential. All
// values come from the latest observed upstream quota snapshot.
func quotaWindowsPayload(auth *coreauth.Auth) []gin.H {
	windows := coreauth.QuotaWindowsForAuth(auth)
	if len(windows) == 0 {
		return nil
	}
	payload := make([]gin.H, 0, len(windows))
	for _, window := range windows {
		item := gin.H{"name": window.Name}
		if window.Duration > 0 {
			item["window_minutes"] = int64(window.Duration / time.Minute)
		}
		if !window.ResetAt.IsZero() {
			item["reset_at"] = window.ResetAt.UTC()
		}
		if window.Status != "" {
			item["status"] = window.Status
		}
		if window.UsedPercent != nil {
			item["used_percent"] = *window.UsedPercent
		}
		payload = append(payload, item)
	}
	return payload
}

// quotaResetPayload renders the long-window reset used by soonest-reset routing.
func quotaResetPayload(auth *coreauth.Auth, now time.Time) gin.H {
	window, ok := coreauth.LongWindowQuotaReset(auth, now)
	if !ok {
		return nil
	}
	return gin.H{
		"reset_at":       window.ResetAt.UTC(),
		"window":         window.Name,
		"window_minutes": int64(window.Duration / time.Minute),
		"observed_at":    auth.Quota.ObservedAt.UTC(),
	}
}

func tokenUsagePayload(authID string) any {
	snapshot, ok := authusage.Default().Snapshot(authID)
	if !ok {
		return nil
	}
	return snapshot
}

// GetRoutingPreference reports, per quota-observing provider, how credentials
// rank under soonest-reset routing and which one a new session would bind to.
// It is read-only and does not touch the usage queue.
func (h *Handler) GetRoutingPreference(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	strategy := "round-robin"
	sessionAffinity := false
	rebindOn := coreauth.SessionAffinityRebindOnAny
	if h.cfg != nil {
		if normalized, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy); ok {
			strategy = normalized
		}
		sessionAffinity = h.cfg.Routing.SessionAffinity
		if value := strings.ToLower(strings.TrimSpace(h.cfg.Routing.SessionAffinityRebindOn)); value == coreauth.SessionAffinityRebindOnQuota || value == coreauth.SessionAffinityRebindOnNever {
			rebindOn = value
		}
	}

	now := time.Now()
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
		ranked, preferredID := coreauth.PreviewSoonestReset(auths, provider, now)
		candidates := make([]gin.H, 0, len(ranked))
		for index, candidate := range ranked {
			auth := candidate.Auth
			item := gin.H{
				"rank":       index + 1,
				"id":         auth.ID,
				"auth_index": auth.Index,
				"name":       authDisplayName(auth),
				"available":  candidate.Available,
				"disabled":   auth.Disabled,
				"preferred":  auth.ID == preferredID,
			}
			if email := authEmail(auth); email != "" {
				item["email"] = email
			}
			if !candidate.ResetAt.IsZero() {
				item["reset_at"] = candidate.ResetAt.UTC()
				item["window"] = candidate.Window
			}
			candidates = append(candidates, item)
		}
		providers = append(providers, gin.H{
			"provider":     provider,
			"preferred_id": preferredID,
			"candidates":   candidates,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"strategy":                   strategy,
		"strategy_active":            strategy == "soonest-reset",
		"session_affinity":           sessionAffinity,
		"session_affinity_rebind_on": rebindOn,
		"generated_at":               now.UTC(),
		"providers":                  providers,
	})
}

func authDisplayName(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return auth.ID
}
