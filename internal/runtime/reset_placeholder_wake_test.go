package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
)

func TestAutomaticFiveHourWakeAcceptsFullQuotaAfterKnownReset(t *testing.T) {
	for _, backgroundRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "preflight", true: "after quota query"}[backgroundRead], func(t *testing.T) {
			e, host, _, clock := manualWakeFixture(t)
			e.cfg.Enabled = true
			if backgroundRead {
				e.QueueQuotaQuery("a")
				if err := e.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if e.Snapshot().Accounts["a"].FiveHour.State != core.WindowUnknown || len(host.sends) != 0 {
					t.Fatal("quota query activated a window or sent a request")
				}
			}
			addUser(e, clock)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(host.sends) != 1 || host.sends[0].AccountID != "a" {
				account := e.Snapshot().Accounts["a"]
				t.Fatalf("full quota after known reset did not wake: %+v", account.Task)
			}
			if e.Snapshot().Accounts["a"].FiveHour.State != core.WindowActive {
				t.Fatal("actual send response did not verify activation")
			}
			clock.now = clock.now.Add(6 * time.Second)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(host.sends) != 1 {
				t.Fatal("same reset triggered a duplicate wake")
			}
		})
	}
}

func TestAutomaticFullQuotaDoesNotBypassKnownActiveWindow(t *testing.T) {
	e, host, _, clock := manualWakeFixture(t)
	e.cfg.Enabled = true
	a := e.state.Accounts["a"]
	oldReset := a.FiveHourBaseline.StableResetAt
	a.FiveHourBaseline = core.WindowBaseline{CycleID: "still-running", StableResetAt: clock.now.Add(time.Hour)}
	a.Task = &core.MergedTask{AccountID: "a", Generation: "g", State: core.TaskPending, Intents: []core.Intent{{ID: "old-due", Reason: core.ReasonFiveHour, OriginalDueAt: oldReset, RandomizedDueAt: oldReset, Status: core.IntentPending}}}
	e.state.Accounts["a"] = a
	addUser(e, clock)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 0 {
		t.Fatal("full placeholder overrode a confirmed future baseline")
	}
	// A shortened zero-usage countdown is also a running window, not reset proof.
	e.host = controlledHost{Host: host, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		return cpa.ParseQuota([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"limit_window_seconds":18000,"used_percent":0,"reset_after_seconds":17999}}}`), read, clock.now, core.ObservationQuery, e.state.Accounts[read.AccountID])
	}}
	clock.now = clock.now.Add(time.Minute)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 0 {
		t.Fatal("shortened countdown was treated as a reset")
	}
}

func TestAutomaticFullQuotaDoesNotVerifyOrRepeatUnconfirmedSend(t *testing.T) {
	e, host, store, clock := manualWakeFixture(t)
	e.cfg.Enabled = true
	reader := e.host
	e.host = controlledHost{Host: reader, send: func(_ context.Context, request core.ExactRequest) (core.SendResult, error) {
		if !store.dispatchSeen {
			t.Fatal("send did not have a durable intent")
		}
		host.sends = append(host.sends, request)
		return core.SendResult{Disposition: core.SendUnknown}, nil
	}}
	addUser(e, clock)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 || e.Snapshot().Accounts["a"].Task.OperationID == "" {
		t.Fatal("full reset quota did not dispatch once and retain delivery suppression")
	}
	restarted, err := New(e.cfg, e.host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		clock.now = clock.now.Add(time.Minute)
		if err := restarted.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		a := restarted.Snapshot().Accounts["a"]
		if len(host.sends) != 1 || a.Task.OperationID == "" || a.FiveHour.State != core.WindowUnknown {
			t.Fatal("full quota was used to confirm activation or repeat an unknown send")
		}
	}
}

func TestRetainedFiveHourWakeWithClearedBaselineDoesNotWaitForNextWeek(t *testing.T) {
	e, host, _, clock := manualWakeFixture(t)
	e.cfg.Enabled = true
	a := e.state.Accounts["a"]
	reset := a.FiveHourBaseline.StableResetAt
	a.WeeklyBaseline = core.WindowBaseline{CycleID: "expired-week", StableResetAt: clock.now.Add(-time.Second)}
	a.Task = &core.MergedTask{AccountID: "a", Generation: "g", State: core.TaskPending, Intents: []core.Intent{{ID: "retained-reset", Reason: core.ReasonFiveHour, CycleID: "old-short", OriginalDueAt: reset, RandomizedDueAt: reset, Status: core.IntentPending}}}
	e.state.Accounts["a"] = a
	permission := core.PermissionAllowed
	// Reproduce the real reducer transition: verified joint reset retires the
	// short baseline but keeps the ordinary intent without a mature replacement.
	core.ApplyObservation(&e.state, core.QuotaObservation{AccountID: "a", Generation: "g", Class: core.ObservationQuery, ReadStartedSeq: core.NextSequence(&e.state), ReceivedAt: clock.now, Permission: &permission,
		FiveHour: &core.WindowEvidence{State: core.WindowInactive, ResetAt: clock.now.Add(5 * time.Hour)},
		Weekly:   &core.WindowEvidence{State: core.WindowInactive, ResetAt: clock.now.Add(7 * 24 * time.Hour)}})
	if !e.state.Accounts["a"].FiveHourBaseline.StableResetAt.IsZero() || e.state.Accounts["a"].Task.Intents[0].Status != core.IntentPending {
		t.Fatal("joint reset did not produce a retained ordinary task")
	}
	core.ApplyObservation(&e.state, core.QuotaObservation{AccountID: "a", Generation: "g", Class: core.ObservationQuery, ReadStartedSeq: core.NextSequence(&e.state), ReceivedAt: clock.now,
		Weekly: &core.WindowEvidence{State: core.WindowActive, ResetAt: clock.now.Add(24 * time.Hour), CycleID: "next-week"}})
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	addUser(e, clock)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 || host.sends[0].AccountID != "a" {
		t.Fatalf("retained five-hour reset could not dispatch without its cleared baseline: %+v", e.Snapshot().Accounts["a"].Task)
	}
}

func TestFullQuotaWithoutFiveHourHistoryDoesNotInventResetWake(t *testing.T) {
	e, host, _, clock := manualWakeFixture(t)
	e.cfg.Enabled = true
	e.cfg.ResetRules.Weekly.Enabled = true
	a := e.state.Accounts["a"]
	a.FiveHourBaseline = core.WindowBaseline{}
	a.WeeklyBaseline = core.WindowBaseline{CycleID: "future-week", StableResetAt: clock.now.Add(24 * time.Hour)}
	a.Task = nil
	e.state.Accounts["a"] = a
	addUser(e, clock)
	for index := 0; index < 2; index++ {
		e.QueueQuotaQuery("a")
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(host.sends) != 0 || !e.Snapshot().Accounts["a"].FiveHourBaseline.StableResetAt.IsZero() {
			t.Fatal("a first full placeholder invented five-hour reset history or sent a model")
		}
		if wake := e.SchedulerDiagnostics().(SchedulingSummary).Plan.NextWakes["a"]; wake.Trigger != string(core.ReasonWeekly) {
			t.Fatalf("unknown five-hour history changed the known weekly plan: %+v", wake)
		}
		clock.now = clock.now.Add(time.Minute)
	}
}
