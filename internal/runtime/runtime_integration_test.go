//go:build linux

package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/storage"
)

// integrationStoreFailure loses the post-send result transition while leaving
// the prior durable dispatch intent on disk, as a crash at this boundary would.
type integrationStoreFailure struct {
	*storage.FileStore
	failNext bool
}

func (s *integrationStoreFailure) Commit(ctx context.Context, rev uint64, next core.CoreState) (uint64, error) {
	if s.failNext {
		s.failNext = false
		return 0, errors.New("injected post-send persistence failure")
	}
	return s.FileStore.Commit(ctx, rev, next)
}

type integrationHost struct {
	t                  *testing.T
	clock              *testClock
	disk               *storage.FileStore
	accounts           []core.HostAccount
	active             map[core.AccountID]bool
	reset              time.Time
	sends              []core.ExactRequest
	reads              int
	disposition        core.SendDisposition
	includeObservation bool
	afterSend          func()
}

func (h *integrationHost) Capabilities(context.Context) (core.HostCapabilities, error) {
	return core.HostCapabilities{StableAccountIdentity: true, ExactAccountExecution: true, QuotaAdapterReady: true}, nil
}
func (h *integrationHost) Accounts(context.Context) ([]core.HostAccount, error) {
	return append([]core.HostAccount(nil), h.accounts...), nil
}
func (h *integrationHost) quota(id core.AccountID, generation core.CredentialGeneration) core.QuotaObservation {
	permission := core.PermissionAllowed
	window := core.WindowEvidence{State: core.WindowExpired, ResetAt: h.reset, Remaining: &core.QuotaAmount{Value: 0, Unit: "percent"}}
	if h.active[id] {
		window.State = core.WindowActive
		window.ResetAt = h.reset.Add(6 * time.Hour)
		window.Remaining.Value = 100
	}
	weekly := core.WindowEvidence{State: core.WindowActive, ResetAt: h.reset.Add(7 * 24 * time.Hour), Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}}
	return core.QuotaObservation{AccountID: id, Generation: generation, ReceivedAt: h.clock.now, Permission: &permission, FiveHour: &window, Weekly: &weekly}
}
func (h *integrationHost) ObserveQuota(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
	h.reads++
	return h.quota(read.AccountID, read.Generation), nil
}
func (h *integrationHost) SendExact(ctx context.Context, request core.ExactRequest) (core.SendResult, error) {
	state, err := h.disk.Load(ctx)
	if err != nil {
		h.t.Fatalf("host cannot read durable dispatch intent: %v", err)
	}
	account := state.Accounts[request.AccountID]
	if account.Task == nil || account.Task.State != core.TaskDispatchIntent || account.Task.OperationID != request.OperationID || account.Generation != request.Generation || account.Task.DispatchIntentAt.IsZero() {
		h.t.Fatalf("network send preceded durable exact-account intent: %#v", account)
	}
	h.sends = append(h.sends, request)
	if h.afterSend != nil {
		h.afterSend()
	}
	result := core.SendResult{Disposition: h.disposition}
	if h.includeObservation {
		h.active[request.AccountID] = true
		observation := h.quota(request.AccountID, request.Generation)
		result.Observation = &observation
	}
	if h.disposition == core.SendUnknown {
		return result, errors.New("upstream outcome unknown")
	}
	return result, nil
}

func integrationFixture(t *testing.T, count int, dailyFloor int) (*Engine, *integrationHost, *storage.FileStore, string, config.Config, *testClock) {
	t.Helper()
	clock := &testClock{dailyInstant(t, "2026-10-11T10:00:00+08:00")}
	cfg := config.Defaults()
	cfg.Execution.Model = "codex-integration-test"
	cfg.ResetRules.Weekly.Enabled = false
	cfg.ResetRules.FiveHour.RandomDelay = config.DelayRange{}
	if dailyFloor > 0 {
		cfg.ResetRules.FiveHour.Enabled = false
		cfg.DailyRules = []config.DailyRule{{ID: "morning", Enabled: true, RandomTimeRange: "09:00-09:00", MinimumReadyAccounts: dailyFloor, CatchUp: true}}
	}
	dir := filepath.Join(t.TempDir(), "durable-state")
	disk, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	initial := core.CoreState{Accounts: map[core.AccountID]core.AccountState{}}
	host := &integrationHost{t: t, clock: clock, disk: disk, active: map[core.AccountID]bool{}, reset: clock.now.Add(-time.Minute), disposition: core.SendUnknown}
	for i := 0; i < count; i++ {
		id := core.AccountID(string(rune('a' + i)))
		initial.Accounts[id] = core.AccountState{ID: id, Generation: "generation-1", Enabled: true, FiveHourBaseline: core.WindowBaseline{CycleID: "prior-short-window", StableResetAt: host.reset}}
		host.accounts = append(host.accounts, core.HostAccount{ID: id, Generation: "generation-1", Enabled: true})
	}
	if _, err := disk.Commit(context.Background(), 0, initial); err != nil {
		t.Fatal(err)
	}
	engine, err := New(cfg, host, disk, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	return engine, host, disk, dir, cfg, clock
}

func integrationReopen(t *testing.T, disk *storage.FileStore, dir string, cfg config.Config, host *integrationHost, clock *testClock) (*Engine, *storage.FileStore) {
	t.Helper()
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	host.disk = reopened
	engine, err := New(cfg, host, reopened, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	return engine, reopened
}

func TestDiskIntegrationLostSendResultDoesNotResendAfterRestart(t *testing.T) {
	engine, host, disk, dir, cfg, clock := integrationFixture(t, 1, 0)
	failing := &integrationStoreFailure{FileStore: disk}
	engine.store = failing
	host.afterSend = func() { failing.failNext = true }
	addUser(engine, clock)
	if err := engine.Step(context.Background()); err == nil {
		t.Fatal("post-send persistence failure was not reported")
	}
	if len(host.sends) != 1 {
		t.Fatalf("send count = %d", len(host.sends))
	}
	persisted, err := disk.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operation := host.sends[0].OperationID
	task := persisted.Accounts["a"].Task
	if task == nil || task.State != core.TaskDispatchIntent || task.OperationID != operation || task.SendOutcome != "" {
		t.Fatalf("lost pre-send recovery fence: %#v", task)
	}
	if err := engine.Step(context.Background()); err == nil || len(host.sends) != 1 {
		t.Fatal("failed writer continued sending")
	}
	// The capacity reservation expires, but the operation's delivery uncertainty
	// must still suppress sending the same account again.
	clock.now = clock.now.Add(20 * time.Minute)
	host.afterSend = nil
	restored, reopened := integrationReopen(t, disk, dir, cfg, host, clock)
	addUser(restored, clock)
	if err := restored.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 {
		t.Fatal("recovery resent an ambiguous operation")
	}
	persisted, err = reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Reservations) != 0 || persisted.Accounts["a"].Task.OperationID != operation {
		t.Fatalf("capacity expiry erased delivery fence: %#v", persisted)
	}
}

func TestDiskIntegrationDailyFloorSubtractsReadyAccountsWithoutUsers(t *testing.T) {
	engine, host, disk, _, _, clock := integrationFixture(t, 2, 2)
	host.active["b"] = true
	host.disposition = core.SendCompleted
	host.includeObservation = true
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 || host.sends[0].AccountID != "a" {
		t.Fatalf("daily floor sends = %#v", host.sends)
	}
	persisted, err := disk.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if core.ActiveUserCount(persisted, clock.now, 30*time.Minute) != 0 {
		t.Fatal("fixture unexpectedly counted an active user")
	}
	if len(persisted.DailyIntents) != 1 || persisted.DailyIntents[0].Status != core.IntentSatisfied || persisted.Accounts["a"].FiveHour.State != core.WindowActive || persisted.Accounts["b"].FiveHour.State != core.WindowActive {
		t.Fatalf("daily floor did not complete durably: %#v", persisted)
	}
	clock.now = clock.now.Add(6 * time.Second)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 {
		t.Fatal("satisfied daily floor produced extra requests")
	}
}

func TestDiskIntegrationDailyPolicyChangePreservesAmbiguousOperation(t *testing.T) {
	for _, mutation := range []string{"deleted", "satisfied"} {
		t.Run(mutation, func(t *testing.T) {
			engine, host, disk, dir, cfg, clock := integrationFixture(t, 2, 1)
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(host.sends) != 1 {
				t.Fatal("initial daily operation was not sent")
			}
			operation := host.sends[0].OperationID
			if mutation == "deleted" {
				cfg.DailyRules = nil
				if err := engine.UpdateConfig(cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				host.active["b"] = true
			}
			clock.now = clock.now.Add(31 * time.Second)
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := disk.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			desiredStatus := core.IntentSuperseded
			if mutation == "satisfied" {
				desiredStatus = core.IntentSatisfied
			}
			if state.DailyIntents[0].Status != desiredStatus {
				t.Fatalf("pool status = %s, want %s", state.DailyIntents[0].Status, desiredStatus)
			}
			task := state.Accounts["a"].Task
			if task == nil || task.OperationID != operation || task.Intents[0].Status != core.IntentPending {
				t.Fatalf("pool policy erased ambiguous covered event: %#v", task)
			}
			restored, reopened := integrationReopen(t, disk, dir, cfg, host, clock)
			if err := restored.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err = reopened.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.Accounts["a"].Task.OperationID != operation || len(host.sends) != 1 {
				t.Fatal("policy-change recovery lost the delivery fence")
			}
		})
	}
}

func TestDiskIntegrationVerificationUsesAbsoluteDelaysAcrossRestart(t *testing.T) {
	engine, host, disk, dir, cfg, clock := integrationFixture(t, 1, 0)
	// A completed response without quota evidence requires verification; it does
	// not prove a new window and must not become a resend after restart.
	host.disposition = core.SendCompleted
	addUser(engine, clock)
	anchor := clock.now
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchedule := func(disk *storage.FileStore, attempt int, offset time.Duration) {
		t.Helper()
		state, err := disk.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		task := state.Accounts["a"].Task
		if task == nil || !task.VerificationStartedAt.Equal(anchor) || task.VerificationAttempt != attempt || !task.NextVerificationAt.Equal(anchor.Add(offset)) {
			t.Fatalf("verification schedule attempt %d / offset %s: %#v", attempt, offset, task)
		}
	}
	assertSchedule(disk, 0, 5*time.Second)
	reads := host.reads
	clock.now = anchor.Add(4 * time.Second)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != reads {
		t.Fatal("verification queried before its absolute due time")
	}
	clock.now = anchor.Add(5 * time.Second)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchedule(disk, 1, 30*time.Second)
	clock.now = anchor.Add(30 * time.Second)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchedule(disk, 2, 120*time.Second)
	clock.now = anchor.Add(31 * time.Second)
	restored, reopened := integrationReopen(t, disk, dir, cfg, host, clock)
	assertSchedule(reopened, 2, 120*time.Second)
	if err := restored.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchedule(reopened, 2, 120*time.Second)
	clock.now = anchor.Add(120 * time.Second)
	if err := restored.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchedule(reopened, 3, 240*time.Second)
	if len(host.sends) != 1 {
		t.Fatal("verification sent another model request")
	}
}
