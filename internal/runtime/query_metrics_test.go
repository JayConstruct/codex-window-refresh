package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type measuredCommitStore struct{ *memoryStore }

func (store measuredCommitStore) Commit(ctx context.Context, revision uint64, next core.CoreState) (uint64, error) {
	// Simulate the real disk work that must be reflected in visible latency.
	time.Sleep(3 * time.Millisecond)
	return store.memoryStore.Commit(ctx, revision, next)
}

func TestManualQueryTimingIncludesQueueAndCommitAndStopsAtCompletion(t *testing.T) {
	e, _, store, clock := fixture(t, false, false)
	if err := e.UpdateConfig(config.Defaults()); err != nil {
		t.Fatal(err)
	}
	e.store = measuredCommitStore{store}
	_, id := e.QueueQuotaQueryJob("a")
	acceptedAt := clock.now
	e.queueMu.Lock()
	e.manualAcceptedAt = time.Now().Add(-2 * time.Second)
	e.queueMu.Unlock()
	clock.now = clock.now.Add(time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, e, id)
	if job.Phase != "completed" || job.QuotaQuery.Timing == nil || !job.QuotaQuery.StartedAt.Equal(acceptedAt) {
		t.Fatalf("acceptance timing lost: %+v", job)
	}
	timing := *job.QuotaQuery.Timing
	if timing.QueuedMS < 2000 || timing.CommitMS < 1 || timing.TotalMS < timing.QueuedMS+timing.CommitMS || timing.InventoryMS < 0 || timing.QueryMS < 0 {
		t.Fatalf("incomplete latency breakdown: %+v", timing)
	}
	// Later foreground evidence commits independently of the completed query.
	clock.now = clock.now.Add(time.Minute)
	e.EnqueueObservation(core.QuotaObservation{AccountID: "a", Generation: "g", ReceivedAt: clock.now,
		FiveHour: &core.WindowEvidence{State: core.WindowActive, ResetAt: clock.now.Add(time.Hour)}})
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if later := findJob(t, e, id).QuotaQuery.Timing; *later != timing {
		t.Fatalf("finished task duration kept advancing: before=%+v after=%+v", timing, *later)
	}
	// Returned timing pointers are detached from published legacy and retained views.
	job.QuotaQuery.Timing.TotalMS = -1
	view := e.ManagementSnapshot()
	view.Scheduler.QuotaQuery.Timing.TotalMS = -2
	if next := e.ManagementSnapshot(); next.Scheduler.QuotaQuery.Timing.TotalMS < 0 || next.Scheduler.Jobs[0].QuotaQuery.Timing.TotalMS < 0 {
		t.Fatal("reader mutated published timing")
	}
}

func TestUnavailableQueryTimingDoesNotGrowOnLaterCommit(t *testing.T) {
	e, host, _, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	e.caps.QuotaAdapterReady = false
	e.host = unreadyLatencyHost{host}
	_, id := e.QueueQuotaQueryJob("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := findJob(t, e, id)
	if first.Phase != "unavailable" || first.QuotaQuery.Timing == nil {
		t.Fatal("unavailable adapter did not finish")
	}
	timing := *first.QuotaQuery.Timing
	e.host = host
	e.EnqueueActivity(core.ActivityEvent{RequestID: "later", Identity: "user", Phase: core.ActivityCompletionOnly, At: e.clock.Now()})
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if next := findJob(t, e, id); *next.QuotaQuery.Timing != timing {
		t.Fatal("unavailable task timing changed after another operation")
	}
}

type unreadyLatencyHost struct{ core.Host }

func (host unreadyLatencyHost) Capabilities(ctx context.Context) (core.HostCapabilities, error) {
	caps, err := host.Host.Capabilities(ctx)
	caps.QuotaAdapterReady = false
	return caps, err
}
