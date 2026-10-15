package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type inventoryRefreshHost struct {
	*fakeHost
	err error
}

func (h *inventoryRefreshHost) Accounts(ctx context.Context) ([]core.HostAccount, error) {
	if h.err != nil {
		return nil, h.err
	}
	return h.fakeHost.Accounts(ctx)
}

func TestRefreshAccountsBypassesInventoryCacheWithoutUpstreamWork(t *testing.T) {
	e, host, store, clock := fixture(t, false, true)
	// Both automatic quota querying and model wake rules are enabled and due.
	// The explicit inventory refresh must never run either kind of work.
	a := e.state.Accounts["a"]
	a.FiveHour.State, a.FiveHour.ResetAt = core.WindowActive, clock.now.Add(time.Hour)
	e.state.Accounts["a"] = a
	if err := e.commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := e.ManagementSnapshot()
	e.inventory = append([]core.HostAccount{}, host.initial...)
	e.nextInventory = clock.now.Add(time.Hour)
	paused := true
	e.cfg.AccountOverrides["paused"] = config.AccountOverride{Paused: &paused}
	host.initial = []core.HostAccount{
		{ID: "a", Generation: "g", Availability: core.AccountDisabled},
		{ID: "new", Generation: "g2", Enabled: true, Availability: core.AccountAvailable},
		{ID: "paused", Generation: "g3", Enabled: true, Availability: core.AccountAvailable},
	}
	if err := e.RefreshAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := e.ManagementSnapshot()
	if view.State.Accounts["a"].Enabled || view.State.Accounts["a"].Availability != core.AccountDisabled ||
		!reflect.DeepEqual(view.State.Accounts["a"].FiveHour, before.State.Accounts["a"].FiveHour) ||
		!view.State.Accounts["new"].Enabled || view.State.Accounts["paused"].Enabled {
		t.Fatalf("refresh lost quota evidence or host/policy state: %+v", view.State.Accounts)
	}
	if view.SnapshotSeq <= before.SnapshotSeq || view.State.Revision <= before.State.Revision || store.s.Accounts["a"].Enabled {
		t.Fatal("updated inventory was not durably published")
	}
	if host.reads != 0 || len(host.sends) != 0 || len(view.Scheduler.Jobs) != 0 || len(view.State.History) != 0 || view.State.Accounts["a"].Task != nil {
		t.Fatal("local refresh performed quota, model, or scheduler work")
	}
	select {
	case <-e.wake:
		t.Fatal("local refresh notified the execution loop")
	default:
	}
	// Re-enabling, rebinding, and removing accounts use the same authoritative
	// reconciliation as the scheduler, without retaining old credential evidence.
	host.initial = []core.HostAccount{{ID: "a", Generation: "replacement", Enabled: true, Availability: core.AccountAvailable}}
	if err := e.RefreshAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := e.Snapshot()
	if len(state.Accounts) != 1 || !state.Accounts["a"].Enabled || state.Accounts["a"].Generation != "replacement" || !state.Accounts["a"].FiveHour.ResetAt.IsZero() {
		t.Fatal("refresh retained removed accounts or quota from an old binding")
	}
}

func TestRefreshAccountsFailurePreservesPublishedAccountsAndCanRecover(t *testing.T) {
	e, host, _, clock := fixture(t, false, true)
	wrapped := &inventoryRefreshHost{fakeHost: host, err: errors.New("private callback failure")}
	e.host = wrapped
	before := e.Snapshot()
	if err := e.RefreshAccounts(context.Background()); err == nil || err.Error() != "account_read_failed" {
		t.Fatalf("unexpected refresh error: %v", err)
	}
	if !reflect.DeepEqual(e.Snapshot(), before) {
		t.Fatal("failed inventory read replaced published state")
	}
	wrapped.err = nil
	e.inventoryFailed, e.nextInventory = true, clock.now.Add(time.Hour)
	if err := e.RefreshAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.inventoryFailed || e.Status().LastError != "" || host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("explicit refresh failed to recover local inventory independently")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	before = e.Snapshot()
	if err := e.RefreshAccounts(cancelled); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(e.Snapshot(), before) {
		t.Fatal("cancelled refresh changed account state")
	}
}
