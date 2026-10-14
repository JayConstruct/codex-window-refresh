package core

import (
	"testing"
	"time"
)

func recoveryFixture() (CoreState, time.Time) {
	state, now := reducerState()
	short := WindowEvidence{State: WindowActive, CycleID: "short", ResetAt: now.Add(time.Hour), Remaining: &QuotaAmount{Value: 20, Unit: "percent"}}
	week := WindowEvidence{State: WindowActive, CycleID: "week", ResetAt: now.Add(24 * time.Hour), Remaining: &QuotaAmount{Value: 40, Unit: "percent"}}
	observe(&state, now, &short, &week)
	return state, now.Add(time.Second)
}

func recoveryObservation(state *CoreState, now time.Time) QuotaObservation {
	account := state.Accounts["a"]
	short, week := account.FiveHour, account.Weekly
	short.Remaining, week.Remaining = &QuotaAmount{Value: 100, Unit: "percent"}, &QuotaAmount{Value: 100, Unit: "percent"}
	permission := PermissionAllowed
	return QuotaObservation{AccountID: "a", Generation: "g1", Class: ObservationQuery, Source: "cpa_wham_usage", ReadStartedSeq: NextSequence(state), ReceivedAt: now, Permission: &permission, FiveHour: &short, Weekly: &week}
}

func TestDoubleFullImmediatelyClearsTimersAndRetiresOnlyUnsentCycleTasks(t *testing.T) {
	for _, sent := range []bool{false, true} {
		state, now := recoveryFixture()
		account := state.Accounts["a"]
		account.Task = &MergedTask{AccountID: "a", Generation: "g1", State: TaskPending, Intents: []Intent{
			{ID: "five", Reason: ReasonFiveHour, CycleID: "short", Status: IntentPending},
			{ID: "week", Reason: ReasonWeekly, CycleID: "week", Status: IntentPending},
			{ID: "daily", Reason: ReasonDaily, Status: IntentPending},
		}}
		if sent {
			account.Task.OperationID = "sent"
			account.Task.State = TaskAwaitingEvidence
			account.Task.CoveredEventIDs = []EventID{"five"}
		}
		state.Accounts["a"] = account
		observation := recoveryObservation(&state, now)
		ApplyObservation(&state, observation)
		account = state.Accounts["a"]
		if account.FiveHour.State != WindowInactive || account.Weekly.State != WindowInactive || !account.FiveHourBaseline.StableResetAt.IsZero() || !account.WeeklyBaseline.StableResetAt.IsZero() || !account.EarlyRecoveryAt.Equal(now) {
			t.Fatal("double full retained old countdown")
		}
		if len(state.History) != 1 || state.History[0].Kind != "reset" || len(state.History[0].Sources) != 2 {
			t.Fatal("recovery did not retain both evidence sources")
		}
		if account.Task.Intents[1].Status != IntentSuperseded || account.Task.Intents[2].Status != IntentPending {
			t.Fatal("recovery retired unrelated daily intent or kept obsolete weekly task")
		}
		if sent && (account.Task.OperationID != "sent" || account.Task.Intents[0].Status != IntentPending) {
			t.Fatal("recovery lifted delivery suppression")
		}
		if !sent && account.Task.Intents[0].Status != IntentSuperseded {
			t.Fatal("recovery retained unsent short task")
		}
		observation.ReadStartedSeq = NextSequence(&state)
		observation.ReceivedAt = now.Add(time.Second)
		ApplyObservation(&state, observation)
		if len(state.History) != 1 {
			t.Fatal("duplicate full observation logged another recovery")
		}
		if err := GenerateResetIntents(&state, "a", now, allResetPolicy(), &fixedRandom{}); err != nil {
			t.Fatal(err)
		}
		if len(state.Accounts["a"].Task.Intents) != 3 {
			t.Fatal("recovery created new wake intent")
		}
	}
}

func TestRecoveryRejectsPartialRoundedStaleBlockedAndUnprovenSnapshots(t *testing.T) {
	for _, scenario := range []string{"single", "rounded", "old_sequence", "old_time", "generation", "permission", "unknown", "no_prior_usage"} {
		t.Run(scenario, func(t *testing.T) {
			state, now := recoveryFixture()
			observation := recoveryObservation(&state, now)
			switch scenario {
			case "single":
				observation.Weekly = nil
			case "rounded":
				observation.Weekly.Remaining.Value = 99.999
			case "old_sequence":
				observation.ReadStartedSeq = 1
			case "old_time":
				observation.ReceivedAt = now.Add(-2 * time.Second)
			case "generation":
				observation.Generation = "new"
			case "permission":
				permission := PermissionBlocked
				observation.Permission = &permission
			case "unknown":
				account := state.Accounts["a"]
				account.Weekly.State = WindowUnknown
				account.WeeklyBaseline.LastConsumedAt = time.Time{}
				state.Accounts["a"] = account
			case "no_prior_usage":
				account := state.Accounts["a"]
				account.Weekly.Remaining = &QuotaAmount{Value: 100, Unit: "percent"}
				account.WeeklyBaseline.LastConsumedAt = time.Time{}
				state.Accounts["a"] = account
			}
			ApplyObservation(&state, observation)
			if !state.Accounts["a"].EarlyRecoveryAt.IsZero() || len(state.History) != 0 || state.Accounts["a"].WeeklyBaseline.StableResetAt.IsZero() {
				t.Fatal("unreliable snapshot reset timers")
			}
		})
	}
}

func TestMissingPermissionDoesNotForgetConsumedCycleBeforeReliableRecovery(t *testing.T) {
	state, now := recoveryFixture()
	observation := recoveryObservation(&state, now)
	observation.Permission = nil
	observation.FiveHour.State, observation.Weekly.State = WindowUnknown, WindowUnknown
	ApplyObservation(&state, observation)
	if len(state.History) != 0 {
		t.Fatal("partial permission reset cycle")
	}
	permission := PermissionAllowed
	observation.Permission = &permission
	observation.ReadStartedSeq, observation.ReceivedAt = NextSequence(&state), now.Add(time.Second)
	ApplyObservation(&state, observation)
	if len(state.History) != 1 || state.Accounts["a"].FiveHour.State != WindowInactive {
		t.Fatal("partial full snapshot erased confirmed consumed baseline")
	}
}
