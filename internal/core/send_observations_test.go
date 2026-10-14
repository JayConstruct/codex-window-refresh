package core

import (
	"testing"
	"time"
)

func TestSendResultAppliesPartialObservationsInOrderWithOriginalTimes(t *testing.T) {
	state, now := dispatchFixture(t)
	shortTime, weekTime := now.Add(time.Second), now.Add(2*time.Second)
	short := WindowEvidence{State: WindowActive, CycleID: "new-short", ResetAt: now.Add(time.Hour), OperationID: "op"}
	week := WindowEvidence{State: WindowActive, CycleID: "new-week", ResetAt: now.Add(24 * time.Hour), OperationID: "op"}
	observations := []QuotaObservation{
		{AccountID: "a", Generation: "g1", ReceivedAt: shortTime, FiveHour: &short},
		{AccountID: "a", Generation: "g1", ReceivedAt: weekTime, Weekly: &week},
	}
	// The compatibility field must not be applied again after the ordered list.
	poison := observations[1]
	poison.FiveHour = &WindowEvidence{State: WindowUnknown}
	if !ApplySendResult(&state, "a", "op", SendResult{Disposition: SendUnknown, Observations: observations, Observation: &poison}, now.Add(3*time.Second)) {
		t.Fatal("send result was rejected")
	}
	account := state.Accounts["a"]
	if account.FiveHour.State != WindowActive || account.Weekly.State != WindowActive || !account.FiveHour.ConfirmedAt.Equal(shortTime) || !account.Weekly.ConfirmedAt.Equal(weekTime) || account.FiveHour.AcceptedSeq >= account.Weekly.AcceptedSeq || account.FiveHour.OperationID != "op" || account.Weekly.OperationID != "op" {
		t.Fatalf("original evidence lost, reordered or refreshed: %+v %+v", account.FiveHour, account.Weekly)
	}
	if account.Task.State != TaskSatisfied {
		t.Fatal("positively evidenced activation was lost after an unknown/error send result")
	}
}

func TestSendResultPartialMessagesCannotFabricateDualWindowProof(t *testing.T) {
	for _, scenario := range []string{"early_recovery", "joint_reset"} {
		t.Run(scenario, func(t *testing.T) {
			state, now := recoveryFixture()
			account := state.Accounts["a"]
			oldShortReset := account.FiveHour.ResetAt
			account.Task = &MergedTask{AccountID: "a", Generation: "g1", State: TaskDispatchIntent, OperationID: "op", CoveredEventIDs: []EventID{"manual"}, Intents: []Intent{{ID: "manual", Reason: ReasonManual, Status: IntentPending, RandomizedDueAt: now.Add(time.Hour)}}}
			permission := PermissionAllowed
			short, week := account.FiveHour, account.Weekly
			if scenario == "early_recovery" {
				short.Remaining, week.Remaining = &QuotaAmount{Value: 100, Unit: "percent"}, &QuotaAmount{Value: 100, Unit: "percent"}
			} else {
				account.WeeklyBaseline.StableResetAt = now.Add(-time.Minute)
				short.State, short.ResetAt = WindowExpired, now.Add(-time.Hour)
				week.State, week.ResetAt = WindowExpired, now.Add(-time.Minute)
			}
			state.Accounts["a"] = account
			observations := []QuotaObservation{
				{AccountID: "a", Generation: "g1", ReceivedAt: now.Add(time.Second), Permission: &permission, FiveHour: &short},
				{AccountID: "a", Generation: "g1", ReceivedAt: now.Add(2 * time.Second), Permission: &permission, Weekly: &week},
			}
			ApplySendResult(&state, "a", "op", SendResult{Disposition: SendUnknown, Observations: observations}, now.Add(3*time.Second))
			account = state.Accounts["a"]
			if len(state.History) != 0 || !account.EarlyRecoveryAt.IsZero() || account.FiveHour.State != WindowActive || !account.FiveHour.ResetAt.Equal(oldShortReset) || account.FiveHourBaseline.StableResetAt.IsZero() {
				t.Fatal("separate messages fabricated same-message dual-window proof")
			}
		})
	}
}

func TestSendResultActivationStillVerifiesAfterLaterUnknown(t *testing.T) {
	state, now := dispatchFixture(t)
	state.Accounts["a"].Task.Intents = []Intent{{ID: "manual", Reason: ReasonManual, Status: IntentPending, RandomizedDueAt: now}}
	state.Accounts["a"].Task.CoveredEventIDs = []EventID{"manual"}
	active := WindowEvidence{State: WindowActive, CycleID: "new-short", ResetAt: now.Add(time.Hour), OperationID: "op"}
	unknown := WindowEvidence{State: WindowUnknown}
	ApplySendResult(&state, "a", "op", SendResult{Disposition: SendUnknown, Observations: []QuotaObservation{
		{AccountID: "a", Generation: "g1", ReceivedAt: now.Add(time.Second), FiveHour: &active},
		{AccountID: "a", Generation: "g1", ReceivedAt: now.Add(2 * time.Second), FiveHour: &unknown},
	}}, now.Add(3*time.Second))
	if state.Accounts["a"].Task.State != TaskSatisfied {
		t.Fatal("later unknown evidence erased verified activation")
	}
}
