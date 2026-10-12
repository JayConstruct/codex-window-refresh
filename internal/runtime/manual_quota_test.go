package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func TestAutomaticQuotaSwitchIsIndependentFromModelWake(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	e.cfg.Enabled = false
	e.cfg.QuotaQuery.Automatic = false
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 0 || len(h.sends) != 0 {
		t.Fatal("disabled automatic queries performed network/model work")
	}
	e.EnqueueObservation(foregroundQuota(h, "cpa_response_headers"))
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 0 || e.Snapshot().Accounts["a"].FiveHour.Remaining.Value != 75 {
		t.Fatal("disabling queries stopped passive updates")
	}
	c.now = c.now.Add(time.Minute)
	e.cfg.QuotaQuery.Automatic = true
	e.cfg.QuotaQuery.Interval = config.Duration(120 * time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 1 || len(h.sends) != 0 {
		t.Fatal("query-only mode sent a model or skipped query")
	}
	c.now = c.now.Add(119 * time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 1 {
		t.Fatal("configured interval ignored")
	}
	c.now = c.now.Add(time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 2 || len(h.sends) != 0 {
		t.Fatal("automatic query generated a model request")
	}
}
func TestManualSweepQueriesAllAccountsInBatchesWithoutDispatch(t *testing.T) {
	e, h, _, c := fixture(t, true, true) // mature weekly wake rules enabled
	e.cfg.QuotaQuery.Automatic = false
	h.initial = nil
	paused := true
	e.state.Accounts = map[core.AccountID]core.AccountState{}
	for i := 0; i < 6; i++ {
		id := core.AccountID(fmt.Sprintf("account-%d", i))
		h.initial = append(h.initial, core.HostAccount{ID: id, Generation: "g", Enabled: true})
		e.state.Accounts[id] = core.AccountState{ID: id, Generation: "g", Enabled: true, WeeklyBaseline: core.WindowBaseline{CycleID: "due", StableResetAt: c.now.Add(-time.Hour)}}
	}
	h.initial = append(h.initial, core.HostAccount{ID: "disabled-host-account", Generation: "g", Enabled: false})
	e.cfg.AccountOverrides = map[core.AccountID]config.AccountOverride{"account-0": {Paused: &paused}}
	if !e.RequestQuotaQuery() || e.RequestQuotaQuery() {
		t.Fatal("manual jobs did not coalesce")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 4 || len(h.sends) != 0 || e.manualQuery.Phase != "running" || e.manualQuery.Total != 6 {
		t.Fatalf("manual batch entered dispatch or exceeded budget: %+v", e.manualQuery)
	}
	if e.RequestQuotaQuery() {
		t.Fatal("running sweep duplicated")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 6 || len(h.sends) != 0 || e.manualQuery.Completed != 6 || e.manualQuery.Phase != "completed" {
		t.Fatalf("manual sweep did not finish read-only: %+v", e.manualQuery)
	}
	if !e.RequestQuotaQuery() {
		t.Fatal("completed sweep prevented later manual request")
	}
}
func TestManualQueryCancelsWhenPermissionDisabledAndNeverRetriesFailures(t *testing.T) {
	e, h, _, _ := fixture(t, false, false)
	e.cfg.Enabled = false
	e.cfg.QuotaQuery.Automatic = false
	e.RequestQuotaQuery()
	e.cfg.QuotaQuery.Manual = false
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 0 || e.manualQuery.Phase != "cancelled" || len(h.sends) != 0 {
		t.Fatal("disabled manual permission still queried")
	}
	e.cfg.QuotaQuery.Manual = true
	reads := 0
	e.host = controlledHost{Host: h, read: func(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
		reads++
		return core.QuotaObservation{}, errors.New("test failure")
	}}
	e.RequestQuotaQuery()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || e.manualQuery.Failed != 1 || e.manualQuery.Phase != "completed" {
		t.Fatal("failed query was not reported")
	}
	for i := 0; i < 5; i++ {
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 || len(h.sends) != 0 {
		t.Fatal("manual failure retried or generated")
	}
}
func TestConcurrentManualRequestsAreCoalesced(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.RequestQuotaQuery() {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted %d duplicate queries", accepted)
	}
}
func TestDisablingManualQueriesCancelsRemainderOfRunningSweep(t *testing.T) {
	e, h, _, _ := fixture(t, false, false)
	e.cfg.Enabled = false
	e.cfg.QuotaQuery.Automatic = false
	h.initial = nil
	for i := 0; i < 6; i++ {
		h.initial = append(h.initial, core.HostAccount{ID: core.AccountID(fmt.Sprint(i)), Generation: "g", Enabled: true})
	}
	e.RequestQuotaQuery()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 4 || e.manualQuery.Phase != "running" {
		t.Fatal("sweep did not start")
	}
	cfg := e.cfg
	cfg.QuotaQuery.Manual = false
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 4 || e.manualQuery.Phase != "cancelled" || len(h.sends) != 0 {
		t.Fatal("disabled query continued or dispatched")
	}
}
