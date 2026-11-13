package auth

import "time"

// SessionBindingRecord is a persistable session affinity binding: one alias
// group bound to a credential until ExpiresAt.
type SessionBindingRecord struct {
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Aliases   []string  `json:"aliases"`
	SessionID string    `json:"session_id,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Sequence  uint64    `json:"sequence,omitempty"`
}

// Snapshot returns the unexpired bindings in eviction order (oldest first).
func (c *SessionCache) Snapshot(now time.Time) []SessionBindingRecord {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.evictionOrder == nil {
		return nil
	}
	records := make([]SessionBindingRecord, 0, len(c.groups))
	for elem := c.evictionOrder.Front(); elem != nil; elem = elem.Next() {
		primaryKey, _ := elem.Value.(string)
		group, ok := c.groups[primaryKey]
		if !ok || !now.Before(group.expiresAt) || group.authID == "" || len(group.aliases) == 0 {
			continue
		}
		records = append(records, SessionBindingRecord{
			AuthID:    group.authID,
			ExpiresAt: group.expiresAt,
			Aliases:   append([]string(nil), group.aliases...),
			SessionID: group.binding.sessionID,
			Provider:  group.binding.provider,
			Sequence:  group.binding.sequence,
		})
	}
	return records
}

// Restore re-inserts persisted bindings. Expired records and records that
// overlap a live binding are skipped, so bindings made after startup win.
// It returns the number of restored groups.
func (c *SessionCache) Restore(records []SessionBindingRecord, now time.Time) int {
	if c == nil || len(records) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	restored := 0
	for _, record := range records {
		if record.AuthID == "" || !now.Before(record.ExpiresAt) {
			continue
		}
		aliases := compactSessionAliases(mergeSessionAliases(nil, record.Aliases...))
		if len(aliases) == 0 {
			continue
		}
		overlaps := false
		for _, alias := range aliases {
			if _, ok := c.entries[alias]; ok {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		binding := sessionBinding{}
		if record.SessionID != "" && record.Provider != "" && record.Sequence != 0 {
			binding = sessionBinding{sessionID: record.SessionID, provider: record.Provider, sequence: record.Sequence}
			if record.Sequence > c.bindingSequence {
				c.bindingSequence = record.Sequence
			}
		}
		c.replaceAliasGroupsLocked(record.AuthID, record.ExpiresAt, aliases, binding)
		restored++
	}
	return restored
}

// SnapshotSessionBindings exports the live session bindings of the selector.
func (s *SessionAffinitySelector) SnapshotSessionBindings(now time.Time) []SessionBindingRecord {
	if s == nil {
		return nil
	}
	return s.cache.Snapshot(now)
}

// RestoreSessionBindings imports persisted session bindings into the selector.
func (s *SessionAffinitySelector) RestoreSessionBindings(records []SessionBindingRecord, now time.Time) int {
	if s == nil {
		return 0
	}
	return s.cache.Restore(records, now)
}
