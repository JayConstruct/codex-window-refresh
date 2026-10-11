package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type controlledHost struct {
	core.Host
	read     func(context.Context, core.QuotaRead) (core.QuotaObservation, error)
	send     func(context.Context, core.ExactRequest) (core.SendResult, error)
	accounts func(context.Context) ([]core.HostAccount, error)
}

func (h controlledHost) ObserveQuota(ctx context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
	if h.read != nil {
		return h.read(ctx, q)
	}
	return h.Host.ObserveQuota(ctx, q)
}
func (h controlledHost) SendExact(ctx context.Context, q core.ExactRequest) (core.SendResult, error) {
	if h.send != nil {
		return h.send(ctx, q)
	}
	return h.Host.SendExact(ctx, q)
}
func (h controlledHost) Accounts(ctx context.Context) ([]core.HostAccount, error) {
	if h.accounts != nil {
		return h.accounts(ctx)
	}
	return h.Host.Accounts(ctx)
}

func TestManagementSnapshotDoesNotWaitForNetwork(t *testing.T) {
	e, h, _, _ := fixture(t, false, false)
	entered, release := make(chan struct{}), make(chan struct{})
	e.host = controlledHost{Host: h, read: func(ctx context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		close(entered)
		<-release
		return h.ObserveQuota(ctx, q)
	}}
	oldRevision := e.Snapshot().Revision
	done := make(chan error, 1)
	go func() { done <- e.Step(context.Background()) }()
	<-entered
	readDone := make(chan core.CoreState, 1)
	go func() {
		_ = e.Status()
		_ = e.SchedulerDiagnostics()
		_, _, _ = e.Diagnostics()
		readDone <- e.Snapshot()
	}()
	select {
	case s := <-readDone:
		if s.Revision != oldRevision {
			t.Error("published uncommitted revision")
		}
	case <-time.After(time.Second):
		t.Error("management waited for quota network")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Revision <= oldRevision {
		t.Fatal("successful transition was not published")
	}
}
func TestFailedQueryWithOldExpiredEvidenceDoesNotRetryEachTick(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	reads := 0
	e.host = controlledHost{Host: h, read: func(ctx context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		reads++
		if reads > 1 {
			return core.QuotaObservation{}, errors.New("unavailable")
		}
		return h.ObserveQuota(ctx, q)
	}}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatalf("expected background+failed preflight, got %d", reads)
	}
	for n := 0; n < 20; n++ {
		c.now = c.now.Add(time.Second)
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 || len(h.sends) != 0 {
		t.Fatal("cached expired evidence bypassed failure backoff")
	}
	c.now = c.now.Add(11 * time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatalf("due retry missing: %d", reads)
	}
	c.now = c.now.Add(31 * time.Second)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatal("repeated failures did not increase backoff")
	}
}
func TestQueryBudgetIncludesCandidatePreflightAndCoversPool(t *testing.T) {
	e, h, _, c := fixture(t, true, true)
	e.state.Accounts = map[core.AccountID]core.AccountState{}
	h.initial = nil
	for n := 0; n < 12; n++ {
		id := core.AccountID(fmt.Sprintf("account-%02d", n))
		a := core.AccountState{ID: id, Generation: "g", Enabled: true, WeeklyBaseline: core.WindowBaseline{CycleID: "week", StableResetAt: c.now.Add(-time.Hour)}, FiveHourBaseline: core.WindowBaseline{CycleID: "short", StableResetAt: c.now.Add(-time.Minute)}}
		e.state.Accounts[id] = a
		h.initial = append(h.initial, core.HostAccount{ID: id, Generation: "g", Enabled: true})
	}
	readCount := 0
	seen := map[core.AccountID]bool{}
	e.host = controlledHost{Host: h, read: func(_ context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		readCount++
		seen[q.AccountID] = true
		o := h.observation(q.AccountID, q.Generation, false)
		o.Weekly = &core.WindowEvidence{State: core.WindowUnknown, ResetAt: c.now.Add(24 * time.Hour)}
		return o, nil
	}}
	for n := 0; n < 6; n++ {
		before := readCount
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if readCount-before > 8 {
			t.Fatalf("quota budget exceeded: %d", readCount-before)
		}
		c.now = c.now.Add(time.Second)
	}
	if len(seen) != 12 {
		t.Fatalf("pool starvation: observed %d of 12", len(seen))
	}
	if len(h.sends) != 0 {
		t.Fatal("unknown weekly reset was sent")
	}
}
func TestConfirmedActiveCapacitySurvivesPreflightFreshnessExpiry(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	h.initial = append(h.initial, core.HostAccount{ID: "b", Generation: "g", Enabled: true})
	e.state.Accounts["b"] = core.AccountState{ID: "b", Generation: "g", Enabled: true, OrdinaryParticipant: true, FiveHour: core.WindowEvidence{State: core.WindowActive, ResetAt: c.now.Add(time.Hour), ConfirmedAt: c.now.Add(-time.Hour)}, Permission: core.PermissionAllowed, PermissionObservedAt: c.now.Add(-time.Hour)}
	e.nextRead["b"] = c.now.Add(time.Hour)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("slow quota polling caused unnecessary extra activation")
	}
	b := e.state.Accounts["b"]
	b.Permission = core.PermissionBlocked
	e.state.Accounts["b"] = b
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("blocked quota did not free capacity")
	}
}
func TestConfiguredDeadlinesReachHostAndCancellationPreservesFence(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	e.cfg.Execution.QuotaReadTimeout = config.Duration(3 * time.Second)
	e.cfg.Execution.RequestTimeout = config.Duration(7 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checkDeadline := func(ctx context.Context, limit time.Duration) {
		t.Helper()
		d, ok := ctx.Deadline()
		if !ok || time.Until(d) > limit || time.Until(d) < limit-time.Second {
			t.Fatalf("host deadline not configured: %v %v", d, ok)
		}
	}
	e.host = controlledHost{Host: h, read: func(readCtx context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		checkDeadline(readCtx, 3*time.Second)
		return h.ObserveQuota(readCtx, q)
	}, send: func(sendCtx context.Context, _ core.ExactRequest) (core.SendResult, error) {
		checkDeadline(sendCtx, 7*time.Second)
		cancel()
		return core.SendResult{Disposition: core.SendCompleted}, nil
	}}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	a := e.Snapshot().Accounts["a"]
	if a.Task == nil || a.Task.OperationID == "" || a.Task.SendOutcome != core.SendUnknown {
		t.Fatal("canceled execution cleared delivery fence")
	}
}
func TestInventoryFailurePersistsActivityAndThrottlesRetry(t *testing.T) {
	e, h, s, c := fixture(t, false, false)
	addUser(e, c)
	calls := 0
	e.host = controlledHost{Host: h, accounts: func(context.Context) ([]core.HostAccount, error) { calls++; return nil, errors.New("missing binding") }}
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("wanted inventory error")
	}
	if core.ActiveUserCount(s.s, c.now, e.cfg.Activity.Lookback.Value()) != 1 {
		t.Fatal("consumed activity not durable on host error")
	}
	c.now = c.now.Add(time.Second)
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("failed inventory must stay fail-closed")
	}
	if calls != 1 || len(h.sends) != 0 {
		t.Fatal("inventory failure retry storm")
	}
}
func TestFailedCommitDoesNotPublishUncommittedDispatch(t *testing.T) {
	e, _, s, c := fixture(t, false, false)
	addUser(e, c)
	s.failDispatch = true
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("wanted durable failure")
	}
	status := e.Status()
	if status.LastError != "durable_commit_failed" {
		t.Fatal("durable failure hidden")
	}
	if a := status.State.Accounts["a"]; a.Task != nil && a.Task.OperationID != "" {
		t.Fatal("management reported an uncommitted dispatch as durable")
	}
}
func TestForcedFreshReadCannotReusePartialWindowCache(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	e.cfg.Execution.QuotaMaxAgeBeforeSend = 0
	reads := 0
	e.host = controlledHost{Host: h, read: func(_ context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
		reads++
		o := h.observation(q.AccountID, q.Generation, false)
		if reads > 1 {
			o.FiveHour = nil
		}
		return o, nil
	}}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("forced read accepted a cached short window absent from preflight response")
	}
}
