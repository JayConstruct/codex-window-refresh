package core

import (
	"testing"
	"time"
)

func reducerState() (CoreState, time.Time) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := CoreState{}
	ReconcileAccounts(&s, []HostAccount{{ID: "a", Generation: "g1", Enabled: true}}, now)
	return s, now
}
func observe(s *CoreState, now time.Time, short, week *WindowEvidence) {
	p := PermissionAllowed
	ApplyObservation(s, QuotaObservation{AccountID: "a", Generation: "g1", ReadStartedSeq: NextSequence(s), ReceivedAt: now, Class: ObservationQuery, Permission: &p, FiveHour: short, Weekly: week})
}
func TestObservationFencesArePerField(t *testing.T) {
	s, now := reducerState()
	oldStart := NextSequence(&s)
	latest := WindowEvidence{State: WindowActive, CycleID: "new", ResetAt: now.Add(time.Hour)}
	observe(&s, now, &latest, nil)
	seq := s.Accounts["a"].FiveHour.AcceptedSeq
	old := WindowEvidence{State: WindowExpired, CycleID: "old", ResetAt: now.Add(-time.Hour)}
	week := WindowEvidence{State: WindowActive, CycleID: "week", ResetAt: now.Add(7 * 24 * time.Hour)}
	if !ApplyObservation(&s, QuotaObservation{AccountID: "a", Generation: "g1", Class: ObservationQuery, ReadStartedSeq: oldStart, ReceivedAt: now.Add(time.Second), FiveHour: &old, Weekly: &week}) {
		t.Fatal("unobserved weekly field should accept old read")
	}
	a := s.Accounts["a"]
	if a.FiveHour.CycleID != "new" || a.FiveHour.AcceptedSeq != seq || a.Weekly.CycleID != "week" {
		t.Fatalf("field fence failed: %+v", a)
	}
	// A long execution response receives a new acceptance fence at completion.
	old.State = WindowActive
	old.CycleID = "execution"
	old.ResetAt = now.Add(2 * time.Hour)
	if !ApplyObservation(&s, QuotaObservation{AccountID: "a", Generation: "g1", Class: ObservationExecution, ReadStartedSeq: oldStart, ReceivedAt: now.Add(2 * time.Second), FiveHour: &old}) || s.Accounts["a"].FiveHour.CycleID != "execution" {
		t.Fatal("execution response incorrectly fenced")
	}
	if ApplyObservation(&s, QuotaObservation{AccountID: "a", Generation: "g0", Class: ObservationExecution, ReceivedAt: now.Add(3 * time.Second), FiveHour: &latest}) {
		t.Fatal("old credentials accepted")
	}
}

type fixedRandom struct {
	calls  int
	amount time.Duration
}

func (r *fixedRandom) Duration(min, max time.Duration) (time.Duration, error) {
	r.calls++
	return r.amount, nil
}
func allResetPolicy() ResetPolicy {
	return ResetPolicy{FiveHourEnabled: true, WeeklyEnabled: true, FiveHourDelay: DelayRange{Max: 10 * time.Minute}, WeeklyDelay: DelayRange{Max: 10 * time.Minute}}
}
func TestStableBaselineRandomAndCatchup(t *testing.T) {
	s, now := reducerState()
	old := WindowEvidence{State: WindowExpired, CycleID: "old", ResetAt: now.Add(-24 * time.Hour)}
	observe(&s, now, &old, &old)
	r := &fixedRandom{amount: 5 * time.Minute}
	if err := GenerateResetIntents(&s, "a", now, allResetPolicy(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 || len(s.Accounts["a"].Task.Intents) != 2 {
		t.Fatal("events not merged")
	}
	deadline := s.Accounts["a"].Task.Intents[0].RandomizedDueAt
	if !deadline.Equal(old.ResetAt.Add(5 * time.Minute)) {
		t.Fatal("catchup delay started at discovery")
	}
	if err := GenerateResetIntents(&s, "a", now.Add(time.Hour), allResetPolicy(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatal("rerolled persistent events")
	}
	// Moving inactive placeholders are not new countdown baselines.
	placeholder := WindowEvidence{State: WindowInactive, ResetAt: now.Add(5 * time.Hour)}
	observe(&s, now.Add(time.Second), &placeholder, nil)
	if s.Accounts["a"].FiveHourBaseline.CycleID != "old" {
		t.Fatal("placeholder changed cycle")
	}
}
func TestJointResetNeverErasesNewActiveWindow(t *testing.T) {
	for _, active := range []bool{false, true} {
		s, now := reducerState()
		old := WindowEvidence{State: WindowExpired, CycleID: "old", ResetAt: now.Add(-time.Minute)}
		observe(&s, now, &old, &old)
		r := &fixedRandom{}
		if err := GenerateResetIntents(&s, "a", now, allResetPolicy(), r); err != nil {
			t.Fatal(err)
		}
		short := WindowEvidence{State: WindowInactive}
		if active {
			short = WindowEvidence{State: WindowActive, CycleID: "new", ResetAt: now.Add(time.Hour)}
		}
		observe(&s, now.Add(time.Second), &short, &old)
		a := s.Accounts["a"]
		if active && a.FiveHourBaseline.CycleID != "new" {
			t.Fatal("joint reset erased active new cycle")
		}
		if !active && !a.FiveHourBaseline.StableResetAt.IsZero() {
			t.Fatal("joint reset retained old short baseline")
		}
		for _, e := range a.Task.Intents {
			if !active && e.Reason == ReasonFiveHour && e.Status != IntentSuperseded {
				t.Fatal("joint reset failed to absorb oldshort")
			}
		}
	}
}
func TestActivityIdempotenceAndRebinding(t *testing.T) {
	s, now := reducerState()
	ApplyActivity(&s, ActivityEvent{RequestID: "unknown", Phase: ActivityCompleted, Identity: "fake", At: now})
	if ActiveUserCount(s, now, 30*time.Minute) != 0 {
		t.Fatal("unknownfinish counted")
	}
	start := ActivityEvent{RequestID: "r1", Phase: ActivityStarted, Identity: "u1", At: now}
	ApplyActivity(&s, start)
	ApplyActivity(&s, start)
	selected := ActivityEvent{RequestID: "r1", Phase: ActivityAccountSelected, AccountID: "a", Generation: "g1", At: now.Add(time.Second)}
	ApplyActivity(&s, selected)
	if !ForegroundBusy(s, "a", "g1") || s.Activity["u1"].InFlightCount != 1 {
		t.Fatal("binding lost")
	}
	selected.AccountID = "b"
	selected.At = now.Add(2 * time.Second)
	ApplyActivity(&s, selected)
	if ForegroundBusy(s, "a", "g1") || !ForegroundBusy(s, "b", "g1") {
		t.Fatal("retry binding not moved")
	}
	finish := ActivityEvent{RequestID: "r1", Phase: ActivityCompleted, At: now.Add(3 * time.Second)}
	ApplyActivity(&s, finish)
	ApplyActivity(&s, finish)
	ApplyActivity(&s, start)
	if ForegroundBusy(s, "b", "g1") || s.Activity["u1"].InFlightCount != 0 || ActiveUserCount(s, now.Add(31*time.Minute), 30*time.Minute) != 0 {
		t.Fatal("duplicate callbacks corrupted count")
	}
	ApplyActivity(&s, ActivityEvent{RequestID: "r2", Phase: ActivityStarted, Identity: "u2", At: now})
	if ActiveUserCount(s, now.Add(2*time.Hour), 30*time.Minute) != 1 {
		t.Fatal("trusted inflight disappeared")
	}
	ExpireActivityRequests(&s, now.Add(2*time.Hour), time.Hour, 24*time.Hour)
	if ActiveUserCount(s, now.Add(2*time.Hour), 30*time.Minute) != 0 {
		t.Fatal("missedfinish never expired")
	}
}
func dispatchFixture(t *testing.T) (CoreState, time.Time) {
	s, now := reducerState()
	old := WindowEvidence{State: WindowExpired, CycleID: "old", ResetAt: now.Add(-time.Hour)}
	observe(&s, now, &old, &old)
	if err := GenerateResetIntents(&s, "a", now, allResetPolicy(), &fixedRandom{}); err != nil {
		t.Fatal(err)
	}
	s.Reservations = []Reservation{{ID: "r", AccountID: "a", Generation: "g1", CapacityHoldUntil: now.Add(time.Minute)}}
	ids := []EventID{}
	for _, e := range s.Accounts["a"].Task.Intents {
		ids = append(ids, e.ID)
	}
	if err := MarkDispatch(&s, "a", "op", "r", ids, now); err != nil {
		t.Fatal(err)
	}
	return s, now
}
func TestUnknownDeliveryCapacityReleaseDoesNotRetry(t *testing.T) {
	s, now := dispatchFixture(t)
	if !ApplySendResult(&s, "a", "op", SendResult{Disposition: SendUnknown}, now) {
		t.Fatal("result not applied")
	}
	ReleaseExpiredReservations(&s, now.Add(2*time.Minute))
	if len(s.Reservations) != 0 || s.Accounts["a"].Task.State != TaskAwaitingEvidence {
		t.Fatal("release lost delivery suppression")
	}
	RecoverAfterRestart(&s, now.Add(3*time.Minute))
	if err := MarkDispatch(&s, "a", "again", "", s.Accounts["a"].Task.CoveredEventIDs, now.Add(3*time.Minute)); err == nil {
		t.Fatal("resent unknown delivery")
	}
	// A short activation alone cannot satisfy an unresolved covered weekly cause.
	short := WindowEvidence{State: WindowActive, CycleID: "newshort", ResetAt: now.Add(5 * time.Hour)}
	observe(&s, now.Add(4*time.Minute), &short, nil)
	if s.Accounts["a"].Task.State != TaskAwaitingEvidence {
		t.Fatal("weekly missing evidence ignored")
	}
	week := WindowEvidence{State: WindowActive, CycleID: "newweek", ResetAt: now.Add(7 * 24 * time.Hour)}
	observe(&s, now.Add(5*time.Minute), nil, &week)
	if s.Accounts["a"].Task.State != TaskSatisfied {
		t.Fatalf("newcycles not verified: %+v", s.Accounts["a"].Task)
	}
}
func TestDefinitelyNotSentKeepsRandomDeadline(t *testing.T) {
	s, now := dispatchFixture(t)
	deadline := s.Accounts["a"].Task.Intents[0].RandomizedDueAt
	ApplySendResult(&s, "a", "op", SendResult{Disposition: SendDefinitelyNotSent, RetryAfter: now.Add(time.Minute)}, now)
	task := s.Accounts["a"].Task
	if task.State != TaskWaiting || task.OperationID != "" || !task.Intents[0].RandomizedDueAt.Equal(deadline) || len(s.Reservations) != 0 {
		t.Fatal("retry transition malformed")
	}
}
func TestRestartPersistedDispatchSuppressesSend(t *testing.T) {
	s, now := dispatchFixture(t)
	RecoverAfterRestart(&s, now.Add(time.Second))
	if s.Accounts["a"].Task.State != TaskAwaitingEvidence || !s.LastDispatchAt.Equal(now) {
		t.Fatal("restart discarded delivery evidence")
	}
	// Credential rotation drops task and reservation, rejecting stale result.
	ReconcileAccounts(&s, []HostAccount{{ID: "a", Generation: "g2", Enabled: true}}, now)
	if s.Accounts["a"].Task != nil || len(s.Reservations) != 0 || ApplySendResult(&s, "a", "op", SendResult{Disposition: SendCompleted}, now) {
		t.Fatal("credential rotation retained operation")
	}
}
func TestImmatureOldCycleSupersededNotExecuted(t *testing.T) {
	s, now := reducerState()
	old := WindowEvidence{State: WindowExpired, CycleID: "old", ResetAt: now.Add(-time.Second)}
	observe(&s, now, &old, nil)
	if err := GenerateResetIntents(&s, "a", now, allResetPolicy(), &fixedRandom{amount: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	active := WindowEvidence{State: WindowActive, CycleID: "new", ResetAt: now.Add(5 * time.Hour)}
	observe(&s, now.Add(time.Second), &active, nil)
	if s.Accounts["a"].Task.Intents[0].Status != IntentSuperseded || s.Accounts["a"].Task.State != TaskSatisfied {
		t.Fatal("foreground activation didn't retire old pending cycle")
	}
}

func TestJointResetPreservesOrdinaryDemandUntilWeeklyReplacementMatures(t *testing.T) {
	tests := []struct {
		name           string
		enabled        bool
		weeklyDeadline time.Duration
		noWeekly       bool
		want           IntentStatus
	}{
		{"disabled weekly policy", false, 0, false, IntentPending},
		{"weekly still waiting for random deadline", true, 5 * time.Minute, false, IntentPending},
		{"no replacement weekly intent", true, 0, true, IntentPending},
		{"mature enabled weekly absorbs old short", true, 0, false, IntentSuperseded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, now := reducerState()
			short := WindowEvidence{State: WindowExpired, CycleID: "short-old", ResetAt: now.Add(-time.Hour)}
			week := WindowEvidence{State: WindowExpired, CycleID: "week-old", ResetAt: now.Add(-time.Minute)}
			observe(&s, now, &short, &week)
			if err := GenerateResetIntents(&s, "a", now, allResetPolicy(), &fixedRandom{}); err != nil {
				t.Fatal(err)
			}
			a := s.Accounts["a"]
			a.WeeklyEnabled = tc.enabled
			if tc.noWeekly {
				out := a.Task.Intents[:0]
				for _, e := range a.Task.Intents {
					if e.Reason != ReasonWeekly {
						out = append(out, e)
					}
				}
				a.Task.Intents = out
			} else {
				for i := range a.Task.Intents {
					if a.Task.Intents[i].Reason == ReasonWeekly {
						a.Task.Intents[i].RandomizedDueAt = now.Add(tc.weeklyDeadline)
					}
				}
			}
			originalShortDeadline := time.Time{}
			for _, e := range a.Task.Intents {
				if e.Reason == ReasonFiveHour {
					originalShortDeadline = e.RandomizedDueAt
				}
			}
			s.Accounts["a"] = a
			inactiveShort := WindowEvidence{State: WindowInactive, ResetAt: now.Add(5 * time.Hour)}
			inactiveWeek := WindowEvidence{State: WindowInactive, ResetAt: now.Add(7 * 24 * time.Hour)}
			observe(&s, now.Add(time.Second), &inactiveShort, &inactiveWeek)
			a = s.Accounts["a"]
			for _, e := range a.Task.Intents {
				if e.Reason == ReasonFiveHour {
					if e.Status != tc.want {
						t.Fatalf("old short status=%s, want %s", e.Status, tc.want)
					}
					if !e.RandomizedDueAt.Equal(originalShortDeadline) {
						t.Fatal("joint reconciliation rerolled short task")
					}
				}
			}
			if !a.FiveHourBaseline.StableResetAt.IsZero() {
				t.Fatal("positive joint reset should retire old local short baseline")
			}
		})
	}
}
