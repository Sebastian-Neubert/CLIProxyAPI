package auth

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// longQuotaWindowMinDuration is the shortest window treated as a long-running
// (weekly-style) subscription quota by soonest-reset routing. Shorter windows
// such as Claude's five-hour or Codex's primary window refill too quickly to
// be worth draining first.
const longQuotaWindowMinDuration = 24 * time.Hour

// QuotaWindow describes one rate-limit window parsed from the latest passive
// quota snapshot of a credential. Every field comes from upstream response
// headers or websocket quota events; nothing is estimated.
type QuotaWindow struct {
	// Name is the provider window identifier: "5h"/"7d" for Claude,
	// "primary"/"secondary" for Codex.
	Name string `json:"name"`
	// Duration is the window length, zero when upstream did not report it.
	Duration time.Duration `json:"-"`
	// ResetAt is the absolute reset time, zero when unknown.
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Status is the provider window status when reported (Claude only).
	Status string `json:"status,omitempty"`
	// UsedPercent is the consumed share in percent, nil when unknown.
	UsedPercent *float64 `json:"used_percent,omitempty"`
}

// QuotaWindowsForAuth parses the quota windows carried by the credential's
// latest quota snapshot. Windows are returned sorted by duration (shortest
// first) and then name.
func QuotaWindowsForAuth(auth *Auth) []QuotaWindow {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return nil
	}
	return parseQuotaWindows(auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt)
}

// LongWindowQuotaReset returns the longest (weekly-style) quota window of the
// credential whose reset is known and still in the future at now.
func LongWindowQuotaReset(auth *Auth, now time.Time) (QuotaWindow, bool) {
	var best QuotaWindow
	found := false
	for _, window := range QuotaWindowsForAuth(auth) {
		if window.Duration < longQuotaWindowMinDuration || window.ResetAt.IsZero() || !window.ResetAt.After(now) {
			continue
		}
		// Prefer the longest window; on equal length the shorter name wins so
		// the generic Claude "7d" window outranks model-scoped variants.
		if !found || window.Duration > best.Duration ||
			(window.Duration == best.Duration && len(window.Name) < len(best.Name)) {
			best = window
			found = true
		}
	}
	return best, found
}

func parseQuotaWindows(provider string, signals map[string]string, observedAt time.Time) []QuotaWindow {
	var windows []QuotaWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows = parseClaudeQuotaWindows(signals)
	case "codex":
		windows = parseCodexQuotaWindows(signals, observedAt)
	default:
		return nil
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Duration != windows[j].Duration {
			return windows[i].Duration < windows[j].Duration
		}
		return windows[i].Name < windows[j].Name
	})
	return windows
}

func parseClaudeQuotaWindows(signals map[string]string) []QuotaWindow {
	names := make(map[string]struct{})
	for key := range signals {
		rest, ok := strings.CutPrefix(strings.ToLower(key), "anthropic-ratelimit-unified-")
		if !ok {
			continue
		}
		for _, field := range []string{"-reset", "-status", "-utilization"} {
			if window, okField := strings.CutSuffix(rest, field); okField && window != "" && claudeWindowDuration(window) > 0 {
				names[window] = struct{}{}
			}
		}
	}
	windows := make([]QuotaWindow, 0, len(names))
	for name := range names {
		window := QuotaWindow{
			Name:     name,
			Duration: claudeWindowDuration(name),
			Status:   strings.TrimSpace(signals[claudeUnifiedWindowHeader(name, "Status")]),
		}
		if resetAt, ok := parseUnixSeconds(signals[claudeUnifiedWindowHeader(name, "Reset")]); ok {
			window.ResetAt = resetAt
		}
		if utilization, err := strconv.ParseFloat(strings.TrimSpace(signals[claudeUnifiedWindowHeader(name, "Utilization")]), 64); err == nil && utilization >= 0 {
			used := utilization * 100
			window.UsedPercent = &used
		}
		windows = append(windows, window)
	}
	return windows
}

// claudeWindowDuration parses Claude window identifiers such as "5h", "7d" or
// "7d_opus". Identifiers without a leading duration (e.g. "overage") are not
// quota windows and return zero.
func claudeWindowDuration(name string) time.Duration {
	end := 0
	for end < len(name) && name[end] >= '0' && name[end] <= '9' {
		end++
	}
	if end == 0 || end >= len(name) {
		return 0
	}
	value, err := strconv.Atoi(name[:end])
	if err != nil || value <= 0 {
		return 0
	}
	if end+1 < len(name) && name[end+1] != '_' {
		return 0
	}
	switch name[end] {
	case 'h':
		return time.Duration(value) * time.Hour
	case 'd':
		return time.Duration(value) * 24 * time.Hour
	default:
		return 0
	}
}

func parseCodexQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	windows := make([]QuotaWindow, 0, 2)
	for _, name := range []string{"primary", "secondary"} {
		prefix := "X-Codex-" + strings.ToUpper(name[:1]) + name[1:] + "-"
		get := func(field string) string {
			return strings.TrimSpace(signals[http.CanonicalHeaderKey(prefix+field)])
		}
		window := QuotaWindow{Name: name}
		present := false
		if minutes, err := strconv.ParseInt(get("Window-Minutes"), 10, 64); err == nil && minutes > 0 {
			window.Duration = time.Duration(minutes) * time.Minute
			present = true
		}
		if resetAt, ok := parseUnixSeconds(get("Reset-At")); ok {
			window.ResetAt = resetAt
			present = true
		} else if seconds, err := strconv.ParseInt(get("Reset-After-Seconds"), 10, 64); err == nil && seconds >= 0 && !observedAt.IsZero() {
			window.ResetAt = observedAt.Add(time.Duration(seconds) * time.Second).Truncate(time.Second)
			present = true
		}
		if used, err := strconv.ParseFloat(get("Used-Percent"), 64); err == nil && used >= 0 {
			window.UsedPercent = &used
			present = true
		}
		if present {
			windows = append(windows, window)
		}
	}
	return windows
}

func parseUnixSeconds(raw string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}
