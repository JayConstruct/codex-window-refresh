package core

import (
	"testing"
	"time"
)

func ordinaryInput() AdmissionInput {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	return AdmissionInput{
		Now: now, Enabled: true, AccountID: "a", Generation: "g1",
		Task: MergedTask{AccountID: "a", Generation: "g1", State: TaskPending,
			Intents: []Intent{{ID: "short", Reason: ReasonFiveHour, Status: IntentPending, RandomizedDueAt: now.Add(-time.Minute)}}},
		FiveHour:        WindowEvidence{State: WindowExpired, ResetAt: now.Add(-time.Minute), ConfirmedAt: now},
		QuotaPermission: PermissionAllowed, QuotaObservedAt: now, QuotaMaxAge: 30 * time.Second,
		WeeklyObservedAt:   now,
		WithinAllowedHours: true, RecentUsers: 2, OrdinarySlotsAvailable: 1, ConcurrencySlotAvailable: true,
	}
}

func TestFullQuotaCanWakeKnownExpiredFiveHourCycle(t *testing.T) {
	for _, scenario := range []string{"reset", "cleared baseline with retained intent", "missing baseline and reset intent", "retired intent", "random delay pending", "daily only", "future baseline", "unrelated intent", "before reset observation", "missing percentage", "partial quota", "short countdown", "stale", "blocked", "no demand", "outside hours", "foreground", "pending delivery", "weekly unconfirmed"} {
		t.Run(scenario, func(t *testing.T) {
			in := ordinaryInput()
			in.FiveHourBaselineResetAt = in.Now.Add(-time.Minute)
			in.Task.Intents[0].OriginalDueAt = in.FiveHourBaselineResetAt
			in.FiveHour = WindowEvidence{Kind: FiveHour, State: WindowUnknown, ResetAt: in.Now.Add(5 * time.Hour), ConfirmedAt: in.Now, Source: "cpa_wham_usage", Remaining: &QuotaAmount{Value: 100, Unit: "percent"}}
			switch scenario {
			case "cleared baseline with retained intent":
				in.FiveHourBaselineResetAt = time.Time{}
			case "missing baseline and reset intent":
				in.FiveHourBaselineResetAt = time.Time{}
				in.Task.Intents[0].OriginalDueAt = time.Time{}
			case "retired intent":
				in.FiveHourBaselineResetAt = time.Time{}
				in.Task.Intents[0].Status = IntentSatisfied
			case "random delay pending":
				in.FiveHourBaselineResetAt = time.Time{}
				in.Task.Intents[0].RandomizedDueAt = in.Now.Add(time.Minute)
			case "daily only":
				in.FiveHourBaselineResetAt = time.Time{}
				in.Task.Intents[0].Reason = ReasonDaily
			case "future baseline":
				in.FiveHourBaselineResetAt = in.Now.Add(time.Hour)
			case "unrelated intent":
				in.Task.Intents[0].OriginalDueAt = in.Now.Add(-time.Hour)
			case "before reset observation":
				in.FiveHourBaselineResetAt = in.Now.Add(-time.Second)
				in.Task.Intents[0].OriginalDueAt = in.FiveHourBaselineResetAt
				in.FiveHour.ConfirmedAt = in.Now.Add(-2 * time.Second)
			case "missing percentage":
				in.FiveHour.Remaining = nil
			case "partial quota":
				in.FiveHour.Remaining.Value = 99
			case "short countdown":
				in.FiveHour.State = WindowActive
				in.FiveHour.ResetAt = in.Now.Add(5*time.Hour - time.Second)
			case "stale":
				in.FiveHour.ConfirmedAt = in.Now.Add(-time.Minute)
			case "blocked":
				in.QuotaPermission = PermissionBlocked
			case "no demand":
				in.OrdinarySlotsAvailable = 0
			case "outside hours":
				in.WithinAllowedHours = false
			case "foreground":
				in.ForegroundBusy = true
			case "pending delivery":
				in.Task.State = TaskAwaitingEvidence
			case "weekly unconfirmed":
				in.Task.Intents = append(in.Task.Intents, Intent{ID: "week", Reason: ReasonWeekly, Status: IntentPending, RandomizedDueAt: in.Now})
			}
			got := DecideAdmission(in)
			if got.Allowed != (scenario == "reset" || scenario == "cleared baseline with retained intent") {
				t.Fatalf("unexpected admission: %+v", got)
			}
		})
	}
}

func TestAdmissionConstraints(t *testing.T) {
	tests := []struct {
		name   string
		change func(*AdmissionInput)
		want   HoldReason
	}{
		{"ordinary eligible", func(*AdmissionInput) {}, HoldNone},
		{"disabled", func(in *AdmissionInput) { in.Enabled = false }, HoldAccountDisabled},
		{"credential changed", func(in *AdmissionInput) { in.Generation = "g2" }, HoldCredentialChanged},
		{"random deadline not yet reached", func(in *AdmissionInput) { in.Task.Intents[0].RandomizedDueAt = in.Now.Add(time.Minute) }, HoldNotDue},
		{"old overdue task catches up", func(in *AdmissionInput) { in.Task.Intents[0].RandomizedDueAt = in.Now.Add(-24 * time.Hour) }, HoldNone},
		{"active window blocks stale quota", func(in *AdmissionInput) {
			in.FiveHour.State = WindowActive
			in.FiveHour.ResetAt = in.Now.Add(time.Hour)
			in.QuotaObservedAt = in.Now.Add(-time.Hour)
		}, HoldActiveFiveHour},
		{"active label past reset needs reclassification", func(in *AdmissionInput) { in.FiveHour.State = WindowActive }, HoldQuotaUnknown},
		{"expired future boundary invalid", func(in *AdmissionInput) { in.FiveHour.ResetAt = in.Now.Add(time.Minute) }, HoldQuotaUnknown},
		{"inactive placeholder future reset accepted", func(in *AdmissionInput) {
			in.FiveHour.State = WindowInactive
			in.FiveHour.ResetAt = in.Now.Add(5 * time.Hour)
		}, HoldNone},
		{"foreground request", func(in *AdmissionInput) { in.ForegroundBusy = true }, HoldForegroundBusy},
		{"stale quota", func(in *AdmissionInput) { in.QuotaObservedAt = in.Now.Add(-time.Minute) }, HoldQuotaUnknown},
		{"fresh permission cannot revive stale short window", func(in *AdmissionInput) { in.FiveHour.ConfirmedAt = in.Now.Add(-time.Minute) }, HoldQuotaUnknown},
		{"missing short window observation time", func(in *AdmissionInput) { in.FiveHour.ConfirmedAt = time.Time{} }, HoldQuotaUnknown},
		{"future short window observation time", func(in *AdmissionInput) { in.FiveHour.ConfirmedAt = in.Now.Add(time.Second) }, HoldQuotaUnknown},
		{"core freshness must be positive", func(in *AdmissionInput) { in.QuotaMaxAge = 0 }, HoldQuotaUnknown},
		{"partial fresh quota cannot refresh old short evidence", func(in *AdmissionInput) { in.FiveHour.ConfirmedAt = in.Now.Add(-time.Minute) }, HoldQuotaUnknown},
		{"clock skew", func(in *AdmissionInput) { in.QuotaObservedAt = in.Now.Add(time.Second) }, HoldQuotaUnknown},
		{"blocked quota", func(in *AdmissionInput) { in.QuotaPermission = PermissionBlocked }, HoldQuotaExhausted},
		{"unknown quota", func(in *AdmissionInput) { in.QuotaPermission = PermissionUnknown }, HoldQuotaUnknown},
		{"night without active users", func(in *AdmissionInput) {
			in.WithinAllowedHours = false
			in.RecentUsers = 0
			in.AllowActiveUsersOutsideHours = true
		}, HoldOutsideHours},
		{"night active users exception", func(in *AdmissionInput) { in.WithinAllowedHours = false; in.AllowActiveUsersOutsideHours = true }, HoldNone},
		{"no capacity", func(in *AdmissionInput) { in.OrdinarySlotsAvailable = 0 }, HoldNoCapacity},
		{"own retained reservation", func(in *AdmissionInput) {
			in.OrdinarySlotsAvailable = 0
			in.OrdinarySlotReserved = true
			in.Task.State = TaskReserved
		}, HoldNone},
		{"no concurrency", func(in *AdmissionInput) { in.ConcurrencySlotAvailable = false }, HoldNoConcurrency},
		{"backoff", func(in *AdmissionInput) { in.BackoffUntil = in.Now.Add(time.Minute) }, HoldBackoff},
		{"dispatch persisted before crash", func(in *AdmissionInput) { in.Task.State = TaskDispatchIntent }, HoldAlreadyDispatched},
		{"await evidence suppresses resend", func(in *AdmissionInput) { in.Task.State = TaskAwaitingEvidence }, HoldAlreadyDispatched},
		{"verifying suppresses resend", func(in *AdmissionInput) { in.Task.State = TaskVerifying }, HoldAlreadyDispatched},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := ordinaryInput()
			tc.change(&in)
			got := DecideAdmission(in)
			if got.Hold != tc.want || got.Allowed != (tc.want == HoldNone) {
				t.Fatalf("got %+v; want hold %q", got, tc.want)
			}
		})
	}
}

func TestWeeklyPriorityAndWindowSafety(t *testing.T) {
	tests := []struct {
		name       string
		resolution WeeklyResolution
		active     bool
		want       HoldReason
	}{
		{"weekly bypasses night and no users", WeeklyConfirmed, false, HoldNone},
		{"weekly must confirm reset", WeeklyUnconfirmed, false, HoldWeeklyUnconfirmed},
		{"weekly mismatch", WeeklyConflict, false, HoldWeeklyConflict},
		{"confirmation cannot clear active short window", WeeklyConfirmed, true, HoldActiveFiveHour},
		{"joint reset conflicts with active short window", WeeklyConflict, true, HoldWeeklyConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := ordinaryInput()
			in.Task.Intents[0].Reason = ReasonWeekly
			in.Weekly = tc.resolution
			in.RecentUsers = 0
			in.WithinAllowedHours = false
			in.OrdinarySlotsAvailable = 0
			if tc.active {
				in.FiveHour.State = WindowActive
				in.FiveHour.ResetAt = in.Now.Add(time.Hour)
			}
			got := DecideAdmission(in)
			if got.Hold != tc.want || got.Allowed != (tc.want == HoldNone) {
				t.Fatalf("got %+v", got)
			}
			if got.Allowed && !got.WeeklyPriority {
				t.Fatal("missing weekly priority")
			}
		})
	}
}

func TestMergeFreezesOnlyMatureEvents(t *testing.T) {
	in := ordinaryInput()
	in.Task.Intents = append(in.Task.Intents,
		Intent{ID: "week", Reason: ReasonWeekly, Status: IntentPending, RandomizedDueAt: in.Now},
		Intent{ID: "daily", Reason: ReasonDaily, Status: IntentPending, RandomizedDueAt: in.Now.Add(time.Minute)},
		in.Task.Intents[0],
	)
	in.Weekly = WeeklyConfirmed
	got := DecideAdmission(in)
	if !got.Allowed || !got.WeeklyPriority || len(got.CoveredEventIDs) != 2 || got.CoveredEventIDs[0] != "short" || got.CoveredEventIDs[1] != "week" {
		t.Fatalf("got %+v", got)
	}
	// An immature weekly reason cannot confer a night/demand exemption.
	in.Task.Intents[1].RandomizedDueAt = in.Now.Add(time.Minute)
	in.WithinAllowedHours = false
	in.RecentUsers = 0
	if got := DecideAdmission(in); got.Hold != HoldOutsideHours {
		t.Fatalf("got %+v", got)
	}
}

func TestDailyAloneCannotBorrowNightException(t *testing.T) {
	in := ordinaryInput()
	in.Task.Intents[0].Reason = ReasonDaily
	in.WithinAllowedHours = false
	in.AllowActiveUsersOutsideHours = true
	if got := DecideAdmission(in); got.Hold != HoldOutsideHours {
		t.Fatalf("got %+v", got)
	}
}

func TestWeeklyEvidenceFreshness(t *testing.T) {
	for _, observedAt := range []time.Time{time.Time{}, ordinaryInput().Now.Add(-time.Minute), ordinaryInput().Now.Add(time.Second)} {
		in := ordinaryInput()
		in.Task.Intents[0].Reason = ReasonWeekly
		in.Weekly = WeeklyConfirmed
		in.WeeklyObservedAt = observedAt
		if got := DecideAdmission(in); got.Hold != HoldQuotaUnknown || got.Allowed {
			t.Fatalf("admitted weekly reset with invalid observation time %v: %+v", observedAt, got)
		}
	}
}
