package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
)

func manualWakeFixture(t *testing.T) (*Engine, *fakeHost, *memoryStore, *testClock) {
	t.Helper()
	engine, host, store, clock := fixture(t, true, false)
	engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
	host.initial = append(host.initial, core.HostAccount{ID: "b", Generation: "other", Enabled: true})
	engine.host = controlledHost{Host: host, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		host.reads++
		return cpa.ParseQuota([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"limit_window_seconds":18000,"used_percent":0,"reset_after_seconds":18000}}}`), read, clock.now, core.ObservationQuery, engine.state.Accounts[read.AccountID])
	}}
	return engine, host, store, clock
}

func TestManualWakeQueriesThenSendsOnlySelectedAccountWithAutomaticWakeOff(t *testing.T) {
	engine, host, store, _ := manualWakeFixture(t)
	if engine.QueueManualWake("a") != "queued" || engine.QueueManualWake("a") != "already_queued" || engine.QueueManualWake("b") != "busy" || engine.QueueQuotaQuery("a") != "busy" {
		t.Fatal("manual wake was widened or duplicated")
	}
	if host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("queue performed network work")
	}
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 1 || len(host.sends) != 1 || host.sends[0].AccountID != "a" || !store.dispatchSeen || engine.cfg.Enabled {
		t.Fatal("manual wake did not stay exact, durable and independent of automatic wake")
	}
	job := engine.SchedulerDiagnostics().(SchedulingSummary).ManualWake
	if job.Phase != "completed" || engine.Snapshot().Accounts["a"].FiveHour.State != core.WindowActive {
		t.Fatalf("actual activation was not verified: %+v", job)
	}
}

func TestFullFiveHourQuotaQueryDoesNotActivateWindowOrSendModel(t *testing.T) {
	engine, host, _, clock := manualWakeFixture(t)
	engine.QueueQuotaQuery("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := engine.Snapshot().Accounts["a"]
	if !core.FullFiveHourPlaceholder(account.FiveHour) || account.FiveHour.State != core.WindowUnknown || account.FiveHourBaseline.StableResetAt.After(clock.now) || len(host.sends) != 0 {
		t.Fatal("query created an active countdown or sent a model")
	}
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 {
		t.Fatal("full unused window could not be explicitly woken")
	}
}

func TestZeroUsageShortCountdownStopsOrVerifiesManualWake(t *testing.T) {
	for _, phase := range []string{"preflight", "response", "verification"} {
		t.Run(phase, func(t *testing.T) {
			engine, _, _, clock := manualWakeFixture(t)
			engine.cfg.Execution.Model = "gpt-5.6-luna"
			delegate := engine.host
			sends := 0
			quota := func(read core.QuotaRead, class core.ObservationClass) (core.QuotaObservation, error) {
				return cpa.ParseQuota([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"limit_window_seconds":18000,"used_percent":0,"reset_after_seconds":17999}}}`), read, clock.now, class, engine.state.Accounts[read.AccountID])
			}
			engine.host = controlledHost{Host: delegate,
				read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
					if phase == "preflight" || sends > 0 {
						return quota(read, core.ObservationQuery)
					}
					return delegate.ObserveQuota(ctx, read)
				},
				send: func(_ context.Context, request core.ExactRequest) (core.SendResult, error) {
					sends++
					if request.Model != "gpt-5.6-luna" {
						t.Fatal("manual wake changed the selected model")
					}
					if phase == "verification" {
						return core.SendResult{Disposition: core.SendUnknown}, nil
					}
					o, err := quota(core.QuotaRead{AccountID: request.AccountID, Generation: request.Generation, ReadStartedSeq: request.ReadStartedSeq}, core.ObservationExecution)
					return core.SendResult{Disposition: core.SendCompleted, Observation: &o}, err
				},
			}
			_, jobID := engine.QueueManualWakeJob("a")
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if phase == "verification" {
				clock.now = clock.now.Add(6 * time.Second)
				if err := engine.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			account := engine.Snapshot().Accounts["a"]
			if account.FiveHour.State != core.WindowActive || account.FiveHour.Remaining.Value != 100 || !account.FiveHour.ResetAt.After(clock.now) {
				t.Fatal("shortened zero-usage countdown did not activate the window")
			}
			if phase == "preflight" {
				if sends != 0 {
					t.Fatal("already active window caused a duplicate model send")
				}
			} else if job := findJob(t, engine, jobID); sends != 1 || job.Phase != "completed" {
				t.Fatalf("manual wake was not verified exactly once: sends=%d phase=%s", sends, job.Phase)
			}
		})
	}
}

func TestManualWakePreservesAllSafetyGates(t *testing.T) {
	for _, scenario := range []string{"paused", "disabled", "model", "host_disabled", "foreground", "active", "unknown", "old_active_baseline", "credential_changed", "backoff", "spacing", "quota_failed", "disk_failed"} {
		t.Run(scenario, func(t *testing.T) {
			engine, host, store, clock := manualWakeFixture(t)
			engine.QueueManualWake("a")
			switch scenario {
			case "paused":
				paused := true
				engine.cfg.AccountOverrides["a"] = config.AccountOverride{Paused: &paused}
			case "disabled":
				host.initial[0].Enabled = false
			case "model":
				engine.cfg.Execution.Model = ""
			case "host_disabled":
				engine.cfg.ManualWakeDisabled = true
			case "foreground":
				engine.EnqueueActivity(core.ActivityEvent{RequestID: "foreground", Identity: "user", AccountID: "a", Generation: "g", Phase: core.ActivityStarted, At: clock.now})
			case "active":
				host.active = true
				engine.host = host
			case "unknown":
				engine.host = controlledHost{Host: host, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
					return cpa.ParseQuota([]byte(`{"rate_limit":{"allowed":true}}`), read, clock.now, core.ObservationQuery, engine.state.Accounts[read.AccountID])
				}}
			case "old_active_baseline":
				account := engine.state.Accounts["a"]
				account.FiveHourBaseline.StableResetAt = clock.now.Add(time.Hour)
				engine.state.Accounts["a"] = account
			case "credential_changed":
				host.initial[0].Generation = "replacement"
			case "backoff":
				account := engine.state.Accounts["a"]
				account.BackoffUntil = clock.now.Add(time.Minute)
				engine.state.Accounts["a"] = account
			case "spacing":
				engine.state.LastDispatchAt = clock.now
			case "quota_failed":
				engine.host = controlledHost{Host: host, read: func(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
					return core.QuotaObservation{}, errors.New("private credentials")
				}}
			case "disk_failed":
				store.failDispatch = true
			}
			err := engine.Step(context.Background())
			if scenario == "disk_failed" && err == nil {
				t.Fatal("failed durable intent did not stop dispatch")
			}
			if scenario != "disk_failed" && err != nil {
				t.Fatal(err)
			}
			if len(host.sends) != 0 || engine.manualWake.Phase == "verifying" {
				t.Fatal("unsafe manual wake sent a request")
			}
		})
	}
}

func TestUnknownManualDeliverySuppressesRetryAcrossRestart(t *testing.T) {
	engine, host, store, clock := manualWakeFixture(t)
	host.unknown = true
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.QueueManualWake("a") != "delivery_evidence_pending" {
		t.Fatal("ambiguous delivery allowed duplicate wake")
	}
	restarted, err := New(engine.cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.QueueManualWake("a") != "delivery_evidence_pending" {
		t.Fatal("restart lost suppression")
	}
	if len(host.sends) != 1 || restarted.Snapshot().Accounts["a"].FiveHour.State == core.WindowActive {
		t.Fatal("ambiguous request fabricated activation or repeated model")
	}
}

func TestConcurrentManualWakeRequestsCoalesce(t *testing.T) {
	engine, _, _, _ := manualWakeFixture(t)
	var workers sync.WaitGroup
	results := make(chan string, 20)
	for worker := 0; worker < 20; worker++ {
		workers.Add(1)
		go func() { defer workers.Done(); results <- engine.QueueManualWake("a") }()
	}
	workers.Wait()
	close(results)
	accepted := 0
	for result := range results {
		if result == "queued" {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent clicks queued multiple wakes")
	}
}

func TestManualWakeCompletedResponseWithoutQuotaDoesNotInventCountdown(t *testing.T) {
	engine, host, _, clock := manualWakeFixture(t)
	readHost := engine.host
	engine.host = controlledHost{Host: readHost, send: func(ctx context.Context, request core.ExactRequest) (core.SendResult, error) {
		result, err := host.SendExact(ctx, request)
		result.Observation = nil
		return result, err
	}}
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := engine.SchedulerDiagnostics().(SchedulingSummary).ManualWake
	if job.Phase != "verifying" || engine.Snapshot().Accounts["a"].FiveHour.State != core.WindowUnknown || engine.QueueManualWake("a") != "delivery_evidence_pending" {
		t.Fatal("successful response invented activation or lost send suppression")
	}
	clock.now = clock.now.Add(time.Second)
	observation := host.observation("a", "g", true)
	observation.Class = core.ObservationExecution
	engine.EnqueueObservation(observation)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.SchedulerDiagnostics().(SchedulingSummary).ManualWake.Phase != "completed" || len(host.sends) != 1 {
		t.Fatal("service activation evidence failed to verify manual wake")
	}
}

func TestDefinitelyNotSentManualWakeDoesNotAutomaticallyRetry(t *testing.T) {
	engine, host, _, clock := manualWakeFixture(t)
	engine.host = controlledHost{Host: engine.host, send: func(context.Context, core.ExactRequest) (core.SendResult, error) {
		return core.SendResult{Disposition: core.SendDefinitelyNotSent}, errors.New("private upstream detail")
	}}
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.manualWake.Phase != "failed" || engine.manualWake.LastError != "send_not_sent" {
		t.Fatal("definitely-not-sent outcome was not reported")
	}
	clock.now = clock.now.Add(2 * time.Minute)
	engine.cfg.Enabled = true
	for cycle := 0; cycle < 3; cycle++ {
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	account := engine.Snapshot().Accounts["a"]
	for _, event := range account.Task.Intents {
		if event.Reason == core.ReasonManual && event.Status == core.IntentPending {
			t.Fatal("unsent manual intent remained eligible for automatic retry")
		}
	}
	if len(host.sends) != 0 {
		t.Fatal("failed explicit wake triggered automatic model dispatch")
	}
}

func TestManualWakeDisabledFlagSurvivesConfigCopyAndReload(t *testing.T) {
	engine, host, store, clock := manualWakeFixture(t)
	settings := engine.cfg
	settings.ManualWakeDisabled = true
	engine, err := New(settings, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.UpdateConfig(settings); err != nil {
		t.Fatal(err)
	}
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 0 || engine.manualWake.LastError != "account_disabled" {
		t.Fatal("disabled host flag was lost during configuration copy")
	}
}
