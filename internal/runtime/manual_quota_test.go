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

func TestDefaultConfigOnlyQueriesOnManualRequest(t *testing.T) {
	e, h, _, clock := fixture(t, true, true)
	if err := e.UpdateConfig(config.Defaults()); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 3; cycle++ {
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.now = clock.now.Add(time.Minute)
	}
	if h.reads != 0 || len(h.sends) != 0 {
		t.Fatal("default config performed automatic queries or model requests")
	}
	e.RequestQuotaQuery()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 1 || len(h.sends) != 0 || e.manualQuery.Phase != "completed" {
		t.Fatal("default config blocked manual query or dispatched model")
	}
}

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
	if h.reads != 4 || len(h.sends) != 0 || e.manualQuery.Phase != "running" || e.manualQuery.Total != 7 {
		t.Fatalf("manual batch entered dispatch or exceeded budget: %+v", e.manualQuery)
	}
	if e.RequestQuotaQuery() {
		t.Fatal("running sweep duplicated")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 7 || len(h.sends) != 0 || e.manualQuery.Completed != 7 || e.manualQuery.Phase != "completed" {
		t.Fatalf("manual sweep did not finish read-only: %+v", e.manualQuery)
	}
	if !e.RequestQuotaQuery() {
		t.Fatal("completed sweep prevented later manual request")
	}
}
func TestManualQueryIgnoresLegacyPermissionAndNeverRetriesFailures(t *testing.T) {
	e, h, _, _ := fixture(t, false, false)
	e.cfg.Enabled = false
	e.cfg.QuotaQuery.Automatic = false
	e.RequestQuotaQuery()
	e.cfg.QuotaQuery.Manual = false
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.reads != 1 || e.manualQuery.Phase != "completed" || len(h.sends) != 0 {
		t.Fatal("legacy manual permission blocked query or dispatched a model")
	}
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
func TestLegacyManualSettingDoesNotCancelRemainderOfRunningSweep(t *testing.T) {
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
	if h.reads != 6 || e.manualQuery.Phase != "completed" || len(h.sends) != 0 {
		t.Fatal("legacy manual setting cancelled sweep or dispatched")
	}
}

func TestAccountQuotaQueryReadsOnlySelectedPausedAccountWithoutDispatch(t *testing.T) {
	e, h, _, _ := fixture(t, true, true) // weekly model wake is due
	e.cfg.QuotaQuery.Automatic = true
	h.initial = append(h.initial, core.HostAccount{ID: "b", Generation: "other", Enabled: true})
	paused := true
	e.cfg.AccountOverrides = map[core.AccountID]config.AccountOverride{"a": {Paused: &paused}}
	var ids []core.AccountID
	e.host = controlledHost{Host: h, read: func(ctx context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		ids = append(ids, q.AccountID)
		return h.ObserveQuota(ctx, q)
	}}
	if e.QueueQuotaQuery("a") != "queued" || e.QueueQuotaQuery("a") != "already_queued" || e.QueueQuotaQuery("b") != "busy" || e.QueueQuotaQuery("") != "busy" {
		t.Fatal("selected query duplicated or widened its target")
	}
	if h.reads != 0 || len(h.sends) != 0 {
		t.Fatal("queue callback performed network work")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := e.SchedulerDiagnostics().(SchedulingSummary).QuotaQuery
	if len(ids) != 1 || ids[0] != "a" || len(h.sends) != 0 || job.AccountID != "a" || job.Total != 1 || job.Completed != 1 || job.Phase != "completed" {
		t.Fatalf("selected query did not stay read-only and bound: ids=%v job=%+v", ids, job)
	}
	if e.QueueQuotaQuery("b") != "queued" {
		t.Fatal("finished query blocked another account")
	}
}

func TestAccountQuotaQueryReadsDisabledAccountButSkipsRemovedAccount(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		e, h, _, _ := fixture(t, false, false)
		e.cfg.QuotaQuery.Automatic = false
		if disabled {
			h.initial[0].Enabled = false
		} else {
			h.initial = []core.HostAccount{}
		}
		e.QueueQuotaQuery("a")
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(h.sends) != 0 || e.manualQuery.Total != 1 || e.manualQuery.Phase != "completed" {
			t.Fatalf("selected query dispatched or did not finish: %+v", e.manualQuery)
		}
		if disabled && (h.reads != 1 || e.manualQuery.Completed != 1 || e.manualQuery.Skipped != 0) {
			t.Fatalf("disabled account blocked read-only query: %+v", e.manualQuery)
		}
		if !disabled && (h.reads != 0 || e.manualQuery.Skipped != 1) {
			t.Fatalf("removed account caused upstream request: %+v", e.manualQuery)
		}
	}
}

func TestAccountQuotaQueryFailureIgnoresLegacyPermission(t *testing.T) {
	for _, manual := range []bool{false, true} {
		e, h, _, _ := fixture(t, false, false)
		e.cfg.Enabled = false
		e.cfg.QuotaQuery.Automatic = false
		reads := 0
		e.host = controlledHost{Host: h, read: func(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
			reads++
			return core.QuotaObservation{}, errors.New("test failure")
		}}
		e.QueueQuotaQuery("a")
		e.cfg.QuotaQuery.Manual = manual
		for i := 0; i < 3; i++ {
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if len(h.sends) != 0 || reads != 1 || e.manualQuery.Failed != 1 || e.manualQuery.Phase != "completed" {
			t.Fatalf("selected query retried, dispatched or was blocked: reads=%d job=%+v", reads, e.manualQuery)
		}
	}
}

func TestManualQuotaQueryDoesNotEnableDisabledAccountOrAutomaticWork(t *testing.T) {
	engine, host, _, _ := fixture(t, false, true)
	host.initial[0].Enabled = false
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("disabled account performed automatic work")
	}
	engine.QueueQuotaQuery("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 1 || engine.state.Accounts["a"].Enabled || engine.manualQuery.Completed != 1 {
		t.Fatal("manual query was blocked or enabled account")
	}
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 1 || len(host.sends) != 0 {
		t.Fatal("manual quota query triggered automatic work")
	}
}

func TestManualQuotaFailurePublishesOnlyFixedCredentialDiagnosis(t *testing.T) {
	for _, message := range []string{"quota_credential_expired", "failure contains a private-token"} {
		e, h, _, _ := fixture(t, false, false)
		e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
		e.host = controlledHost{Host: h, read: func(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
			return core.QuotaObservation{}, errors.New(message)
		}}
		e.QueueQuotaQuery("a")
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		expected := "quota_read_failed"
		if message == "quota_credential_expired" {
			expected = message
		}
		status := e.SchedulerDiagnostics().(SchedulingSummary).QuotaQuery
		if status.LastError != expected || status.Failed != 1 || len(h.sends) != 0 {
			t.Fatal("query diagnostics lost its fixed classification or leaked host errors")
		}
	}
}
