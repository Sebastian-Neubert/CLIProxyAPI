package authusage

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestStoreAggregatesCacheBucketsPerAuth(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return now }
	ctx := context.Background()

	// Claude reports cache reads/writes separately from uncached input.
	store.HandleUsage(ctx, coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now, Detail: coreusage.Detail{
		InputTokens: 100, CacheReadTokens: 9000, CacheCreationTokens: 500, OutputTokens: 300,
	}})
	store.HandleUsage(ctx, coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now.Add(-2 * time.Hour), Detail: coreusage.Detail{
		InputTokens: 50, CacheReadTokens: 1000, OutputTokens: 10,
	}})
	store.HandleUsage(ctx, coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now, Failed: true})
	// Outside the 24h window.
	store.HandleUsage(ctx, coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now.Add(-30 * time.Hour), Detail: coreusage.Detail{
		InputTokens: 1_000_000,
	}})
	store.HandleUsage(ctx, coreusage.Record{AuthID: "", Provider: "claude", RequestedAt: now, Detail: coreusage.Detail{InputTokens: 1}})

	snapshot, ok := store.Snapshot("claude-a")
	if !ok {
		t.Fatal("Snapshot() found no data")
	}
	if got := snapshot.CurrentHour; got.Requests != 2 || got.Failed != 1 || got.InputTokens != 100 || got.CacheReadTokens != 9000 || got.CacheWriteTokens != 500 || got.OutputTokens != 300 {
		t.Fatalf("current hour = %+v", got)
	}
	if got := snapshot.Last24h; got.Requests != 3 || got.InputTokens != 150 || got.CacheReadTokens != 10000 || got.CacheWriteTokens != 500 || got.OutputTokens != 310 {
		t.Fatalf("last 24h = %+v", got)
	}

	// Advancing the clock expires buckets without wall-clock waits.
	now = now.Add(24 * time.Hour)
	snapshot, _ = store.Snapshot("claude-a")
	if got := snapshot.Last24h; got.Requests != 0 || got.CacheReadTokens != 0 {
		t.Fatalf("expired last 24h = %+v, want empty", got)
	}
	if _, ok = store.Snapshot("unknown"); ok {
		t.Fatal("Snapshot(unknown) reported data")
	}
}

func TestStoreDoesNotDoubleCountCodexCachedInput(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return now }
	// OpenAI-style usage reports cached tokens as a subset of input tokens.
	store.HandleUsage(context.Background(), coreusage.Record{AuthID: "codex-a", Provider: "codex", ExecutorType: "codex", RequestedAt: now, Detail: coreusage.Detail{
		InputTokens: 1000, CachedTokens: 800, CacheReadTokens: 800, OutputTokens: 50, TotalTokens: 1050,
	}})
	snapshot, _ := store.Snapshot("codex-a")
	if got := snapshot.CurrentHour; got.InputTokens+got.CacheReadTokens+got.CacheWriteTokens != 1000 || got.CacheReadTokens != 800 {
		t.Fatalf("codex buckets = %+v, want 1000 input split into 200 uncached + 800 cache read", got)
	}
}
