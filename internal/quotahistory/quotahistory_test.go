package quotahistory

import (
	"testing"
	"time"
)

func weekly(used float64, reset time.Time) []Window {
	return []Window{{Name: "7d", WindowMinutes: 10080, UsedPercent: &used, ResetAt: &reset}}
}

func TestRecordSkipsUnchangedAndTooCloseObservations(t *testing.T) {
	store := NewStore()
	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	reset := base.Add(72 * time.Hour)

	if !store.Record("a", base, weekly(10, reset)) {
		t.Fatal("first observation not recorded")
	}
	if store.Record("a", base.Add(10*time.Minute), weekly(10, reset)) {
		t.Fatal("unchanged observation recorded")
	}
	if store.Record("a", base.Add(2*time.Minute), weekly(11, reset)) {
		t.Fatal("observation inside min spacing recorded")
	}
	if !store.Record("a", base.Add(6*time.Minute), weekly(11, reset)) {
		t.Fatal("changed observation after min spacing not recorded")
	}
	if store.Record("a", base.Add(5*time.Minute), weekly(12, reset)) {
		t.Fatal("out-of-order observation recorded")
	}
	// Codex derives resets from relative seconds; sub-minute jitter is no reset.
	if store.Record("a", base.Add(7*time.Minute), weekly(11, reset.Add(20*time.Second))) {
		t.Fatal("reset jitter recorded as change")
	}
	// A reset inside min spacing must still be recorded so the step is visible.
	newReset := reset.Add(7 * 24 * time.Hour)
	if !store.Record("a", base.Add(8*time.Minute), weekly(0, newReset)) {
		t.Fatal("reset inside min spacing not recorded")
	}
	if got := len(store.Series("a", time.Time{})); got != 3 {
		t.Fatalf("points = %d, want 3", got)
	}
}

func TestPruneDropsExpiredPointsAndUnknownAuths(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	store.Record("a", now.Add(-30*24*time.Hour), weekly(1, reset))
	store.Record("a", now.Add(-time.Hour), weekly(2, reset))
	store.Record("gone", now.Add(-time.Hour), weekly(3, reset))
	store.Record("old", now.Add(-29*24*time.Hour), weekly(4, reset))

	store.Prune(now, map[string]struct{}{"a": {}, "old": {}})

	if got := store.Series("a", time.Time{}); len(got) != 1 || *got[0].Windows[0].UsedPercent != 2 {
		t.Fatalf("series a = %+v, want only the recent point", got)
	}
	exported := store.Export()
	if _, ok := exported["gone"]; ok {
		t.Fatal("unknown auth kept")
	}
	if _, ok := exported["old"]; ok {
		t.Fatal("auth with only expired points kept")
	}
}

func TestImportMergesSortedAndSeriesFiltersSince(t *testing.T) {
	store := NewStore()
	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	reset := base.Add(72 * time.Hour)
	store.Record("a", base.Add(2*time.Hour), weekly(30, reset))

	store.Import(map[string][]Point{"a": {
		{T: base.Add(time.Hour), Windows: weekly(20, reset)},
		{T: base, Windows: weekly(10, reset)},
		{T: base.Add(2 * time.Hour), Windows: weekly(99, reset)},
	}})

	series := store.Series("a", time.Time{})
	if len(series) != 3 {
		t.Fatalf("series = %+v, want 3 points", series)
	}
	for i, want := range []float64{10, 20, 30} {
		if got := *series[i].Windows[0].UsedPercent; got != want {
			t.Fatalf("point %d used = %v, want %v (live point must win on equal timestamp)", i, got, want)
		}
	}
	if got := store.Series("a", base.Add(30*time.Minute)); len(got) != 2 {
		t.Fatalf("since filter = %d points, want 2", len(got))
	}
}
