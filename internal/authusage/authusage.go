// Package authusage keeps a small, process-local rolling token aggregate per
// credential for management views (e.g. cache read/write ratio as an
// indicator of working session affinity). It never consumes or mutates the
// usage queue and is lost on restart.
package authusage

import (
	"context"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	bucketWidth = time.Hour
	bucketCount = 24
	maxAuths    = 4096
)

func init() {
	coreusage.RegisterPlugin(defaultStore)
}

// Totals is a token aggregate over a time range. Input buckets are mutually
// exclusive: InputTokens excludes cache reads and cache writes.
type Totals struct {
	Requests         int64 `json:"requests"`
	Failed           int64 `json:"failed"`
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

// Snapshot reports aggregates for one credential: the current clock hour and
// the last 24 hourly buckets including it.
type Snapshot struct {
	CurrentHour Totals `json:"current_hour"`
	Last24h     Totals `json:"last_24h"`
}

type bucket struct {
	start int64
	Totals
}

type authBuckets struct {
	buckets [bucketCount]bucket
}

// Store aggregates usage records per credential ID.
type Store struct {
	mu    sync.Mutex
	auths map[string]*authBuckets
	now   func() time.Time
}

var defaultStore = NewStore()

// NewStore returns an empty aggregate store.
func NewStore() *Store {
	return &Store{auths: make(map[string]*authBuckets)}
}

// Default returns the process-wide store fed by the usage manager.
func Default() *Store { return defaultStore }

func (s *Store) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// HandleUsage implements coreusage.Plugin.
func (s *Store) HandleUsage(_ context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = s.currentTime()
	}
	start := at.Truncate(bucketWidth).Unix()
	breakdown := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown

	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.auths[authID]
	if entry == nil {
		if len(s.auths) >= maxAuths {
			s.auths = make(map[string]*authBuckets)
		}
		entry = &authBuckets{}
		s.auths[authID] = entry
	}
	slot := &entry.buckets[(start/int64(bucketWidth.Seconds()))%bucketCount]
	if slot.start != start {
		if slot.start > start {
			// Older than the retained window for this slot.
			return
		}
		*slot = bucket{start: start}
	}
	slot.Requests++
	if record.Failed {
		slot.Failed++
	}
	if breakdown.Valid() {
		slot.InputTokens += breakdown.Input.UncachedTokens
		slot.CacheReadTokens += breakdown.Input.CacheReadTokens
		slot.CacheWriteTokens += breakdown.Input.CacheWriteTokens
		slot.OutputTokens += breakdown.Output.TotalTokens
	}
}

// Snapshot returns the rolling aggregates for authID.
func (s *Store) Snapshot(authID string) (Snapshot, bool) {
	if s == nil {
		return Snapshot{}, false
	}
	now := s.currentTime()
	currentStart := now.Truncate(bucketWidth).Unix()
	oldest := currentStart - int64((bucketCount-1)*bucketWidth.Seconds())

	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.auths[strings.TrimSpace(authID)]
	if entry == nil {
		return Snapshot{}, false
	}
	var snapshot Snapshot
	found := false
	for _, slot := range entry.buckets {
		if slot.start == 0 || slot.start < oldest || slot.start > currentStart {
			continue
		}
		found = true
		addTotals(&snapshot.Last24h, slot.Totals)
		if slot.start == currentStart {
			addTotals(&snapshot.CurrentHour, slot.Totals)
		}
	}
	return snapshot, found
}

func addTotals(dst *Totals, src Totals) {
	dst.Requests += src.Requests
	dst.Failed += src.Failed
	dst.InputTokens += src.InputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.OutputTokens += src.OutputTokens
}
