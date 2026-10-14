package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

// targetedLatencyHost counts credential refreshes independently of quota reads.
// Embedding the legacy Host interface also exercises the optional Account API.
type targetedLatencyHost struct {
	core.Host
	pool           []core.HostAccount
	lists, targets int
	refresh        func(core.AccountID) (core.HostAccount, error)
}

func (h *targetedLatencyHost) Accounts(context.Context) ([]core.HostAccount, error) {
	h.lists++
	return h.pool, nil
}
func (h *targetedLatencyHost) Account(_ context.Context, id core.AccountID) (core.HostAccount, error) {
	h.targets++
	if h.refresh != nil {
		return h.refresh(id)
	}
	for _, account := range h.pool {
		if account.ID == id {
			return account, nil
		}
	}
	return core.HostAccount{}, errors.New("account_binding_changed")
}

func cachedLatencyFixture(t *testing.T, size int) (*Engine, *fakeHost, *targetedLatencyHost, *testClock) {
	t.Helper()
	e, base, _, clock := fixture(t, true, true)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	host := &targetedLatencyHost{Host: base}
	for index := 0; index < size; index++ {
		host.pool = append(host.pool, core.HostAccount{ID: core.AccountID(fmt.Sprintf("account-%03d", index)), Generation: "g", Enabled: true})
	}
	e.host = host
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.lists, host.targets, base.reads = 0, 0, 0
	return e, base, host, clock
}

func TestSelectedQuotaQueryRefreshCostIndependentOfPoolSize(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			e, base, host, clock := cachedLatencyFixture(t, size)
			// Both expired cache and a previous global refresh failure must leave
			// a known, independently validated account immediately queryable.
			for _, backoff := range []bool{false, true} {
				e.inventoryFailed = backoff
				e.nextInventory = clock.now.Add(-time.Second)
				if backoff {
					e.nextInventory = clock.now.Add(time.Minute)
				}
				beforeTargets, beforeReads := host.targets, base.reads
				e.cfg.Enabled = true
				e.cfg.QuotaQuery.Automatic = true
				if e.QueueQuotaQuery("account-000") != "queued" {
					t.Fatal("query rejected")
				}
				if err := e.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if host.lists != 0 || host.targets-beforeTargets != 1 || base.reads-beforeReads != 1 || len(base.sends) != 0 {
					t.Fatalf("selected query widened work: lists=%d refreshes=%d reads=%d sends=%d", host.lists, host.targets-beforeTargets, base.reads-beforeReads, len(base.sends))
				}
				if query := e.manualQuery; query.Phase != "completed" || query.Completed != 1 || query.Failed != 0 || query.AccountID != "account-000" {
					t.Fatalf("selected query not completed: %+v", query)
				}
			}
		})
	}
}

func TestSelectedQuotaQueryRefreshClearsOldGenerationEvidence(t *testing.T) {
	for _, staleResponse := range []bool{false, true} {
		t.Run(fmt.Sprint(staleResponse), func(t *testing.T) {
			e, base, host, clock := cachedLatencyFixture(t, 2)
			id := core.AccountID("account-000")
			account := e.state.Accounts[id]
			account.FiveHour = core.WindowEvidence{State: core.WindowActive, ResetAt: clock.now.Add(time.Hour), ConfirmedAt: clock.now, Remaining: &core.QuotaAmount{Value: 97}}
			e.state.Accounts[id] = account
			host.refresh = func(target core.AccountID) (core.HostAccount, error) {
				return core.HostAccount{ID: target, Generation: "replacement", Enabled: true}, nil
			}
			var requested core.CredentialGeneration
			host.Host = controlledHost{Host: base, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
				requested = read.Generation
				observation := base.observation(read.AccountID, read.Generation, false)
				if staleResponse {
					observation.Generation = "g"
				}
				return observation, nil
			}}
			e.QueueQuotaQuery(id)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			current := e.Snapshot().Accounts[id]
			if requested != "replacement" || current.Generation != "replacement" || current.FiveHour.Remaining != nil || current.FiveHour.State == core.WindowActive || host.lists != 0 {
				t.Fatalf("replacement reused old credential evidence: requested=%s account=%+v", requested, current)
			}
			if staleResponse && (e.manualQuery.Failed != 1 || e.manualQuery.Completed != 0) {
				t.Fatal("old generation response accepted")
			}
			if !staleResponse && e.manualQuery.Completed != 1 {
				t.Fatal("replacement credential was not queried")
			}
		})
	}
}

func TestSelectedQuotaQueryTargetRefreshFailureTerminatesWithoutNetwork(t *testing.T) {
	e, base, host, _ := cachedLatencyFixture(t, 100)
	host.refresh = func(core.AccountID) (core.HostAccount, error) {
		return core.HostAccount{}, errors.New("account_binding_changed")
	}
	e.QueueQuotaQuery("account-000")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if query := e.manualQuery; query.Phase != "completed" || query.Failed != 1 || query.LastError != "account_binding_changed" || base.reads != 0 || host.lists != 0 || len(base.sends) != 0 {
		t.Fatalf("removed account was queried or left queued: %+v", query)
	}
}

func TestQueuedManualQueryYieldsBackgroundReadsAndModelDispatch(t *testing.T) {
	e, base, host, clock := cachedLatencyFixture(t, 6)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = true, true
	// Expired weekly baselines make automatic model execution immediately due.
	for id, account := range e.state.Accounts {
		account.WeeklyBaseline = core.WindowBaseline{CycleID: "week-old", StableResetAt: clock.now.Add(-time.Hour)}
		e.state.Accounts[id] = account
	}
	var ids []core.AccountID
	host.Host = controlledHost{Host: base, read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		ids = append(ids, read.AccountID)
		if len(ids) == 1 {
			if e.QueueQuotaQuery("account-005") != "queued" {
				t.Error("manual query rejected while background read in flight")
			}
		}
		return base.ObserveQuota(ctx, read)
	}}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || len(base.sends) != 0 {
		t.Fatalf("queued manual work did not yield background cycle: reads=%v sends=%d", ids, len(base.sends))
	}
	if e.Snapshot().Accounts[ids[0]].Weekly.ConfirmedAt.IsZero() {
		t.Fatal("yield discarded completed background evidence")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[1] != "account-005" || len(base.sends) != 0 || e.manualQuery.Completed != 1 || host.targets != 1 || host.lists != 0 {
		t.Fatalf("next step did not prioritize selected quota-only work: reads=%v query=%+v", ids, e.manualQuery)
	}
}

func TestSelectedQuotaQueryFallsBackForUnknownAccountsLegacyHostsAndSweeps(t *testing.T) {
	for _, scenario := range []string{"unknown", "legacy", "sweep"} {
		t.Run(scenario, func(t *testing.T) {
			e, base, host, _ := cachedLatencyFixture(t, 2)
			target := core.AccountID("account-000")
			expectedReads := 1
			switch scenario {
			case "unknown":
				target = "new-account"
				host.pool = append(host.pool, core.HostAccount{ID: target, Generation: "new-generation", Enabled: true})
			case "legacy":
				// A host implementing only the original Host API remains usable.
				e.host = controlledHost{Host: base, accounts: host.Accounts}
			case "sweep":
				target, expectedReads = "", 2
			}
			e.QueueQuotaQuery(target)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if host.lists != 1 || host.targets != 0 || base.reads != expectedReads || e.manualQuery.Completed != expectedReads {
				t.Fatalf("fallback lost inventory synchronization: lists=%d targets=%d reads=%d query=%+v", host.lists, host.targets, base.reads, e.manualQuery)
			}
		})
	}
}

func TestManualQueryQueuedDuringPreflightPreventsModelDispatch(t *testing.T) {
	e, base, host, clock := cachedLatencyFixture(t, 2)
	e.cfg.Enabled = true
	for id, account := range e.state.Accounts {
		account.WeeklyBaseline = core.WindowBaseline{CycleID: "week-old", StableResetAt: clock.now.Add(-time.Hour)}
		e.state.Accounts[id] = account
	}
	var ids []core.AccountID
	host.Host = controlledHost{Host: base, read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		ids = append(ids, read.AccountID)
		if len(ids) == 1 && e.QueueQuotaQuery("account-001") != "queued" {
			t.Error("could not queue query during automatic preflight")
		}
		return base.ObserveQuota(ctx, read)
	}}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || len(base.sends) != 0 {
		t.Fatalf("preflight proceeded to dispatch after manual request: reads=%v sends=%d", ids, len(base.sends))
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[1] != "account-001" || len(base.sends) != 0 || e.manualQuery.Completed != 1 {
		t.Fatal("manual quota request was not next after preflight yielded")
	}
}
