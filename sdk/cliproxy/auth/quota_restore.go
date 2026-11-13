package auth

import (
	"strings"
	"time"
)

// QuotaObservation is a passive quota snapshot of one credential that can be
// persisted across restarts. It carries only header-derived observation data,
// never cooldown or scheduler state.
type QuotaObservation struct {
	Provider   string            `json:"provider,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals"`
}

// QuotaObservations returns the current passive quota snapshot of every
// credential that has one, keyed by auth ID.
func (m *Manager) QuotaObservations() map[string]QuotaObservation {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]QuotaObservation)
	for id, auth := range m.auths {
		if auth == nil || len(auth.Quota.Signals) == 0 || auth.Quota.ObservedAt.IsZero() {
			continue
		}
		signals := make(map[string]string, len(auth.Quota.Signals))
		for key, value := range auth.Quota.Signals {
			signals[key] = value
		}
		out[id] = QuotaObservation{Provider: auth.Provider, ObservedAt: auth.Quota.ObservedAt, Signals: signals}
	}
	return out
}

// RestoreQuotaObservation applies a persisted snapshot to a registered
// credential when it is newer than the live one. It reports whether the
// snapshot was applied, and false when the auth is not registered yet so the
// caller can retry later. The restored data is not written back to the auth
// store.
func (m *Manager) RestoreQuotaObservation(authID string, observation QuotaObservation) (applied bool, registered bool) {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" {
		return false, false
	}
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.Unlock()
		return false, false
	}
	if len(observation.Signals) == 0 || observation.ObservedAt.IsZero() ||
		!ProviderSupportsQuotaObservation(auth.Provider) ||
		(observation.Provider != "" && !strings.EqualFold(observation.Provider, auth.Provider)) ||
		!observation.ObservedAt.After(auth.Quota.ObservedAt) {
		m.mu.Unlock()
		return false, true
	}
	signals := make(map[string]string, len(observation.Signals))
	for key, value := range observation.Signals {
		signals[key] = value
	}
	auth.Quota.Signals = signals
	auth.Quota.ObservedAt = observation.ObservedAt
	snapshot := auth.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return true, true
}
