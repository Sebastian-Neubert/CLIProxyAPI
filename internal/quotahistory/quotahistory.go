// Package quotahistory keeps a bounded per-credential time series of passive
// quota window observations (used percent and reset time per window) for
// management views such as the pooled subscription usage chart.
package quotahistory

import (
	"sort"
	"sync"
	"time"
)

const (
	// DefaultRetention bounds how long points are kept.
	DefaultRetention = 28 * 24 * time.Hour
	// DefaultMinSpacing is the minimum distance between two recorded points of
	// one credential unless a window reset changed.
	DefaultMinSpacing = 5 * time.Minute
	// maxPointsPerAuth caps memory per credential regardless of spacing.
	maxPointsPerAuth = 12000
	// maxAuths caps the number of tracked credentials.
	maxAuths = 1024
)

// Window is one quota window observed at a point in time.
type Window struct {
	Name          string     `json:"name"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
}

// Point is the set of windows of one credential observed at T.
type Point struct {
	T       time.Time `json:"t"`
	Windows []Window  `json:"windows"`
}

// Store is a concurrency-safe per-credential history.
type Store struct {
	mu         sync.Mutex
	series     map[string][]Point
	retention  time.Duration
	minSpacing time.Duration
	dirty      bool
}

var defaultStore = NewStore()

// Default returns the process-wide history store.
func Default() *Store { return defaultStore }

// NewStore returns an empty store with default retention and spacing.
func NewStore() *Store {
	return &Store{series: make(map[string][]Point), retention: DefaultRetention, minSpacing: DefaultMinSpacing}
}

// Record appends an observation for authID when it differs from the last
// recorded point. Observations closer than the minimum spacing to the last
// point are dropped unless a reset time changed, so resets stay visible as
// steps. It reports whether a point was stored.
func (s *Store) Record(authID string, observedAt time.Time, windows []Window) bool {
	if s == nil || authID == "" || observedAt.IsZero() || len(windows) == 0 {
		return false
	}
	windows = cloneWindows(windows)
	sortWindows(windows)
	s.mu.Lock()
	defer s.mu.Unlock()
	points := s.series[authID]
	if points == nil && len(s.series) >= maxAuths {
		return false
	}
	if n := len(points); n > 0 {
		last := points[n-1]
		if !observedAt.After(last.T) || sameWindows(last.Windows, windows) {
			return false
		}
		if observedAt.Sub(last.T) < s.minSpacing && !resetChanged(last.Windows, windows) {
			return false
		}
	}
	points = append(points, Point{T: observedAt.UTC(), Windows: windows})
	if len(points) > maxPointsPerAuth {
		points = append([]Point(nil), points[len(points)-maxPointsPerAuth:]...)
	}
	s.series[authID] = points
	s.dirty = true
	return true
}

// Prune drops points older than the retention window and credentials that
// are no longer known. A nil keep set keeps every credential.
func (s *Store) Prune(now time.Time, keep map[string]struct{}) {
	if s == nil {
		return
	}
	cutoff := now.Add(-s.retention)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, points := range s.series {
		if keep != nil {
			if _, ok := keep[id]; !ok {
				delete(s.series, id)
				s.dirty = true
				continue
			}
		}
		index := sort.Search(len(points), func(i int) bool { return !points[i].T.Before(cutoff) })
		if index == 0 {
			continue
		}
		s.dirty = true
		if index >= len(points) {
			delete(s.series, id)
			continue
		}
		s.series[id] = append([]Point(nil), points[index:]...)
	}
}

// Series returns a copy of the points of authID at or after since.
func (s *Store) Series(authID string, since time.Time) []Point {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	points := s.series[authID]
	index := sort.Search(len(points), func(i int) bool { return !points[i].T.Before(since) })
	out := make([]Point, 0, len(points)-index)
	for _, point := range points[index:] {
		out = append(out, Point{T: point.T, Windows: cloneWindows(point.Windows)})
	}
	return out
}

// Retention reports the configured retention window.
func (s *Store) Retention() time.Duration {
	if s == nil {
		return DefaultRetention
	}
	return s.retention
}

// Export returns a deep copy of every series and clears the dirty flag.
func (s *Store) Export() map[string][]Point {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Point, len(s.series))
	for id, points := range s.series {
		copied := make([]Point, len(points))
		for i, point := range points {
			copied[i] = Point{T: point.T, Windows: cloneWindows(point.Windows)}
		}
		out[id] = copied
	}
	s.dirty = false
	return out
}

// Dirty reports whether points changed since the last Export.
func (s *Store) Dirty() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// Import merges persisted series. Points are sorted and de-duplicated by
// timestamp; live points recorded after startup are kept.
func (s *Store) Import(series map[string][]Point) {
	if s == nil || len(series) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, imported := range series {
		if id == "" || len(imported) == 0 {
			continue
		}
		if s.series[id] == nil && len(s.series) >= maxAuths {
			continue
		}
		merged := make([]Point, 0, len(imported)+len(s.series[id]))
		for _, point := range imported {
			if point.T.IsZero() || len(point.Windows) == 0 {
				continue
			}
			windows := cloneWindows(point.Windows)
			sortWindows(windows)
			merged = append(merged, Point{T: point.T.UTC(), Windows: windows})
		}
		merged = append(merged, s.series[id]...)
		sort.SliceStable(merged, func(i, j int) bool { return merged[i].T.Before(merged[j].T) })
		deduped := merged[:0]
		for _, point := range merged {
			if n := len(deduped); n > 0 && deduped[n-1].T.Equal(point.T) {
				deduped[n-1] = point
				continue
			}
			deduped = append(deduped, point)
		}
		if len(deduped) > maxPointsPerAuth {
			deduped = deduped[len(deduped)-maxPointsPerAuth:]
		}
		if len(deduped) == 0 {
			continue
		}
		s.series[id] = append([]Point(nil), deduped...)
	}
}

func cloneWindows(windows []Window) []Window {
	out := make([]Window, len(windows))
	for i, window := range windows {
		out[i] = window
		if window.UsedPercent != nil {
			used := *window.UsedPercent
			out[i].UsedPercent = &used
		}
		if window.ResetAt != nil {
			resetAt := window.ResetAt.UTC()
			out[i].ResetAt = &resetAt
		}
	}
	return out
}

func sortWindows(windows []Window) {
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].WindowMinutes != windows[j].WindowMinutes {
			return windows[i].WindowMinutes < windows[j].WindowMinutes
		}
		return windows[i].Name < windows[j].Name
	})
}

func sameWindows(left, right []Window) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].WindowMinutes != right[i].WindowMinutes ||
			!sameFloat(left[i].UsedPercent, right[i].UsedPercent) || !sameTime(left[i].ResetAt, right[i].ResetAt) {
			return false
		}
	}
	return true
}

func resetChanged(previous, next []Window) bool {
	byName := make(map[string]*time.Time, len(previous))
	for _, window := range previous {
		byName[window.Name] = window.ResetAt
	}
	for _, window := range next {
		before, ok := byName[window.Name]
		if !ok || !sameResetTime(before, window.ResetAt) {
			return true
		}
	}
	return false
}

// sameResetTime treats reset times within a minute as equal; Codex derives
// them from relative seconds, so they jitter between observations.
func sameResetTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	delta := left.Sub(*right)
	if delta < 0 {
		delta = -delta
	}
	return delta < time.Minute
}

func sameFloat(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return sameResetTime(left, right)
}
