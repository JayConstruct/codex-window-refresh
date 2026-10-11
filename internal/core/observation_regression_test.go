package core

import (
	"testing"
	"time"
)

func TestOlderExpiredShortDoesNotReplaceConfirmedFutureCountdown(t *testing.T) {
	for _, class := range []ObservationClass{ObservationQuery, ObservationExecution} {
		for _, obscureCurrent := range []bool{false, true} {
			t.Run(string(class)+map[bool]string{false: "/active", true: "/unknown"}[obscureCurrent], func(t *testing.T) {
				s, now := reducerState()
				active := WindowEvidence{State: WindowActive, CycleID: "current", ResetAt: now.Add(time.Hour)}
				observe(&s, now, &active, nil)
				if obscureCurrent {
					unknown := WindowEvidence{State: WindowUnknown}
					observe(&s, now.Add(time.Second), &unknown, nil)
				}
				before := s.Accounts["a"].FiveHour
				older := WindowEvidence{State: WindowExpired, CycleID: "previous", ResetAt: now.Add(-time.Hour)}
				p := PermissionAllowed
				ApplyObservation(&s, QuotaObservation{AccountID: "a", Generation: "g1", Class: class, ReadStartedSeq: NextSequence(&s), ReceivedAt: now.Add(2 * time.Second), Permission: &p, FiveHour: &older})
				a := s.Accounts["a"]
				if a.FiveHour.State != before.State || a.FiveHour.AcceptedSeq != before.AcceptedSeq || !a.FiveHourBaseline.StableResetAt.Equal(active.ResetAt) {
					t.Fatal("older expired observation erased an established future countdown")
				}
				if err := GenerateResetIntents(&s, "a", now.Add(2*time.Second), allResetPolicy(), &fixedRandom{}); err != nil {
					t.Fatal(err)
				}
				if s.Accounts["a"].Task != nil {
					t.Fatal("future countdown produced an expired task")
				}
			})
		}
	}
}

func TestOlderExpiredShortCannotUseUnverifiedWeeklyReset(t *testing.T) {
	for _, reason := range []string{"previous-week", "future-expiration", "weekly-fence", "before-week-boundary"} {
		t.Run(reason, func(t *testing.T) {
			s, now := reducerState()
			active := WindowEvidence{State: WindowActive, CycleID: "short-current", ResetAt: now.Add(time.Hour)}
			week := WindowEvidence{State: WindowActive, CycleID: "week-current", ResetAt: now.Add(-time.Minute)}
			if reason == "before-week-boundary" {
				week.ResetAt = now.Add(time.Minute)
			}
			observe(&s, now, &active, &week)
			seq := NextSequence(&s)
			weekReset := WindowEvidence{State: WindowExpired, ResetAt: week.ResetAt}
			switch reason {
			case "previous-week":
				weekReset.ResetAt = week.ResetAt.Add(-7 * 24 * time.Hour)
			case "future-expiration":
				weekReset.ResetAt = now.Add(time.Minute)
			case "weekly-fence":
				observe(&s, now.Add(time.Second), nil, &week)
			case "before-week-boundary":
				weekReset.State = WindowInactive
			}
			older := WindowEvidence{State: WindowExpired, ResetAt: now.Add(-time.Hour)}
			ApplyObservation(&s, QuotaObservation{AccountID: "a", Generation: "g1", Class: ObservationQuery, ReadStartedSeq: seq, ReceivedAt: now.Add(2 * time.Second), FiveHour: &older, Weekly: &weekReset})
			a := s.Accounts["a"]
			if a.FiveHour.State != WindowActive || !a.FiveHourBaseline.StableResetAt.Equal(active.ResetAt) {
				t.Fatal("unverified weekly field invalidated the active short countdown")
			}
		})
	}
}

func TestExpiredShortAcceptedAfterConfirmedCountdownEnds(t *testing.T) {
	s, now := reducerState()
	active := WindowEvidence{State: WindowActive, CycleID: "current", ResetAt: now.Add(time.Hour)}
	observe(&s, now, &active, nil)
	expired := WindowEvidence{State: WindowExpired, CycleID: active.CycleID, ResetAt: active.ResetAt}
	observe(&s, active.ResetAt, &expired, nil)
	if s.Accounts["a"].FiveHour.State != WindowExpired {
		t.Fatal("elapsed confirmed countdown did not accept expiration")
	}
}

func TestVerifiedJointWeeklyResetCanRetireFutureShortCountdown(t *testing.T) {
	for _, weeklyState := range []WindowState{WindowExpired, WindowInactive} {
		t.Run(string(weeklyState), func(t *testing.T) {
			s, now := reducerState()
			active := WindowEvidence{State: WindowActive, CycleID: "short-current", ResetAt: now.Add(time.Hour)}
			week := WindowEvidence{State: WindowActive, CycleID: "week-current", ResetAt: now.Add(time.Minute)}
			observe(&s, now, &active, &week)
			at := now.Add(2 * time.Minute)
			if err := GenerateResetIntents(&s, "a", at, allResetPolicy(), &fixedRandom{}); err != nil {
				t.Fatal(err)
			}
			shortReset := WindowEvidence{State: WindowExpired, CycleID: "reset-short", ResetAt: now.Add(time.Minute)}
			weekReset := WindowEvidence{State: weeklyState, CycleID: "week-reset", ResetAt: week.ResetAt}
			observe(&s, at, &shortReset, &weekReset)
			a := s.Accounts["a"]
			if a.FiveHour.State != WindowExpired || !a.FiveHourBaseline.StableResetAt.IsZero() {
				t.Fatal("verified weekly joint reset failed to retire the old short countdown")
			}
		})
	}
}
