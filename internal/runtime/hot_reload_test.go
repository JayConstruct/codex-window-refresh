package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestHotReloadFlushesQueuesPreservesForegroundAndDoesNotSend(t *testing.T) {
	e, host, store, clock := fixture(t, false, false)
	if hotReloadProcessIdentity() == "" {
		t.Skip("Linux process identity unavailable")
	}
	e.EnqueueActivity(core.ActivityEvent{RequestID: "busy", Identity: "u", AccountID: "a", Generation: "g", Phase: core.ActivityStarted, At: clock.now})
	o := host.observation("a", "g", true)
	o.Class = core.ObservationExecution
	e.EnqueueObservation(o)
	if err := e.PrepareHotReload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.s.Requests["busy"].Completed || store.s.Activity["u"].InFlightCount != 1 || store.s.HotReloadProcess == "" {
		t.Fatal("accepted foreground event was not durably handed off")
	}
	if store.s.Accounts["a"].FiveHour.State != core.WindowActive {
		t.Fatal("pending quota observation lost")
	}
	next, err := New(e.cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Snapshot().Requests["busy"].Completed || next.Snapshot().Activity["u"].InFlightCount != 1 || store.s.HotReloadProcess != "" {
		t.Fatal("same-process reload cleared active foreground occupancy or reused handoff marker")
	}
	next.EnqueueActivity(core.ActivityEvent{RequestID: "busy", Phase: core.ActivityCompleted, At: clock.now.Add(time.Second)})
	if err := next.PrepareHotReload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.s.Requests["busy"].Completed || store.s.Activity["u"].InFlightCount != 0 {
		t.Fatal("new instance did not complete the inherited request")
	}
	if len(host.sends) != 0 || host.reads != 0 {
		t.Fatal("handoff performed upstream work")
	}
}

func TestHotReloadMarkerCannotCarryOccupancyIntoDifferentProcess(t *testing.T) {
	e, host, store, clock := fixture(t, false, false)
	store.s.HotReloadProcess = "different-boot:123:456"
	store.s.Requests["stale"] = core.RequestActivity{Identity: "u", AccountID: "a", Generation: "g"}
	next, err := New(e.cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Snapshot().Requests["stale"].Completed || next.Snapshot().Activity["u"].InFlightCount != 0 {
		t.Fatal("process restart retained stale foreground occupancy")
	}
}

func TestHotReloadRetainsUncertainDeliveryWithoutResending(t *testing.T) {
	e, host, store, clock := fixture(t, false, false)
	if hotReloadProcessIdentity() == "" {
		t.Skip("Linux process identity unavailable")
	}
	account := store.s.Accounts["a"]
	account.Task = &core.MergedTask{State: core.TaskDispatchIntent, OperationID: "already-sent"}
	store.s.Accounts["a"] = account
	store.s.HotReloadProcess = hotReloadProcessIdentity()
	store.s.GenerationJob = &core.PromptGenerationJob{Phase: "dispatched"}
	next, err := New(e.cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	task := next.Snapshot().Accounts["a"].Task
	if task.OperationID != "already-sent" || task.State != core.TaskAwaitingEvidence ||
		next.Snapshot().GenerationJob.Phase != "unknown" || len(host.sends) != 0 {
		t.Fatal("reload discarded delivery uncertainty or sent a duplicate")
	}
}

type cancelOnCommitStore struct {
	*memoryStore
	cancel context.CancelFunc
}

func (s cancelOnCommitStore) Commit(ctx context.Context, revision uint64, state core.CoreState) (uint64, error) {
	s.cancel() // Simulate quiesce arriving after commit has already begun.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.memoryStore.Commit(ctx, revision, state)
}

func TestQuiesceCancellationDuringCommitDoesNotPoisonRuntime(t *testing.T) {
	e, _, store, _ := fixture(t, false, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.store = cancelOnCommitStore{store, cancel}
	if err := e.commit(ctx); err != nil || e.poisoned || ctx.Err() == nil {
		t.Fatalf("commit interrupted by normal lifecycle cancellation: %v", err)
	}
}
