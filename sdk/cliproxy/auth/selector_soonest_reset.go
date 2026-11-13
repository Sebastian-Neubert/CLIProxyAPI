package auth

import (
	"context"
	"sort"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector prefers the available credential whose long-running
// (weekly-style) subscription quota resets first, so quota that would expire
// unused is consumed before quota that stays valid for longer.
//
// Reset times come only from the latest upstream quota snapshot of each
// credential (Claude anthropic-ratelimit-unified-* headers, Codex x-codex-*
// headers or websocket rate-limit events). Credentials without a known future
// reset rank after every credential with one and rotate round-robin among
// themselves, so the pool learns their reset times quickly. Availability,
// priority tiers, cooldowns and disabled credentials are handled exactly like
// the built-in selectors; session affinity wraps this selector as usual and
// keeps precedence for bound sessions.
type SoonestResetSelector struct {
	unknown RoundRobinSelector
	mu      sync.Mutex
	now     func() time.Time
}

// NewSoonestResetSelector returns a selector implementing the soonest-reset strategy.
func NewSoonestResetSelector() *SoonestResetSelector {
	return &SoonestResetSelector{}
}

func (s *SoonestResetSelector) currentTime() time.Time {
	s.mu.Lock()
	nowFn := s.now
	s.mu.Unlock()
	if nowFn != nil {
		return nowFn()
	}
	return time.Now()
}

// Pick selects the available credential with the earliest known long-window reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.currentTime()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ranked := RankAuthsBySoonestReset(available, now)
	if len(ranked) > 0 && !ranked[0].ResetAt.IsZero() {
		return ranked[0].Auth, nil
	}
	return s.unknown.Pick(ctx, provider, model, opts, available)
}

// SoonestResetCandidate is one credential ranked by RankAuthsBySoonestReset.
type SoonestResetCandidate struct {
	Auth *Auth
	// ResetAt is the known future long-window reset, zero when unknown.
	ResetAt time.Time
	// Window is the quota window ResetAt belongs to.
	Window string
}

// RankAuthsBySoonestReset orders credentials by their earliest known future
// long-window reset. Credentials with an unknown reset follow, ordered by ID.
// The input slice is not modified.
func RankAuthsBySoonestReset(auths []*Auth, now time.Time) []SoonestResetCandidate {
	ranked := make([]SoonestResetCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		candidate := SoonestResetCandidate{Auth: auth}
		if window, ok := LongWindowQuotaReset(auth, now); ok {
			candidate.ResetAt = window.ResetAt
			candidate.Window = window.Name
		}
		ranked = append(ranked, candidate)
	}
	sortSoonestResetCandidates(ranked)
	return ranked
}

func sortSoonestResetCandidates(ranked []SoonestResetCandidate) {
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		aKnown, bKnown := !a.ResetAt.IsZero(), !b.ResetAt.IsZero()
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown && !a.ResetAt.Equal(b.ResetAt) {
			return a.ResetAt.Before(b.ResetAt)
		}
		return a.Auth.ID < b.Auth.ID
	})
}

// SoonestResetPreviewCandidate extends a ranked credential with its
// credential-level availability for management views.
type SoonestResetPreviewCandidate struct {
	SoonestResetCandidate
	// Available reports whether the credential is selectable right now in the
	// highest available priority tier (model-specific cooldowns are ignored).
	Available bool
}

// PreviewSoonestReset ranks one provider's credentials the way
// SoonestResetSelector would for a model-agnostic request. preferredID is the
// credential a new session would bind to; it is empty when no available
// credential has a known reset, in which case unknown credentials rotate.
func PreviewSoonestReset(auths []*Auth, provider string, now time.Time) (ranked []SoonestResetPreviewCandidate, preferredID string) {
	availableIDs := make(map[string]struct{})
	if available, err := getAvailableAuths(auths, provider, "", now); err == nil {
		for _, auth := range available {
			availableIDs[auth.ID] = struct{}{}
		}
	}
	for _, candidate := range RankAuthsBySoonestReset(auths, now) {
		_, available := availableIDs[candidate.Auth.ID]
		ranked = append(ranked, SoonestResetPreviewCandidate{SoonestResetCandidate: candidate, Available: available})
		if available && preferredID == "" && !candidate.ResetAt.IsZero() {
			preferredID = candidate.Auth.ID
		}
	}
	return ranked, preferredID
}
