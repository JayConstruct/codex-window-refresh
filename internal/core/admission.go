package core

import "time"

type HoldReason string

const (
	HoldNone              HoldReason = ""
	HoldAccountDisabled   HoldReason = "account_disabled"
	HoldCredentialChanged HoldReason = "credential_changed"
	HoldNotDue            HoldReason = "not_due"
	HoldAlreadyDispatched HoldReason = "delivery_evidence_pending"
	HoldActiveFiveHour    HoldReason = "active_five_hour"
	HoldQuotaUnknown      HoldReason = "quota_unknown"
	HoldQuotaExhausted    HoldReason = "quota_exhausted"
	HoldWeeklyUnconfirmed HoldReason = "weekly_reset_unconfirmed"
	HoldWeeklyConflict    HoldReason = "weekly_reset_conflict"
	HoldForegroundBusy    HoldReason = "foreground_busy"
	HoldBackoff           HoldReason = "backoff"
	HoldOutsideHours      HoldReason = "outside_hours"
	HoldNoCapacity        HoldReason = "target_satisfied"
	HoldNoConcurrency     HoldReason = "no_concurrency_slot"
)

type WeeklyResolution string

const (
	WeeklyUnconfirmed WeeklyResolution = "unconfirmed"
	WeeklyConfirmed   WeeklyResolution = "confirmed"
	WeeklyConflict    WeeklyResolution = "conflict"
)

type AdmissionInput struct {
	Now                          time.Time
	Enabled                      bool
	AccountID                    AccountID
	Generation                   CredentialGeneration
	Task                         MergedTask
	FiveHour                     WindowEvidence
	FiveHourBaselineResetAt      time.Time
	QuotaPermission              Permission
	QuotaObservedAt              time.Time
	QuotaMaxAge                  time.Duration
	Weekly                       WeeklyResolution
	WeeklyObservedAt             time.Time
	ForegroundBusy               bool
	BackoffUntil                 time.Time
	WithinAllowedHours           bool
	RecentUsers                  int
	AllowActiveUsersOutsideHours bool
	// Computed in the same serialized arbitration turn. An owned slot is valid
	// only if reconciliation retained it; do not reject it merely because P now
	// includes this reservation and therefore Missing is zero.
	OrdinarySlotReserved     bool
	OrdinarySlotsAvailable   int
	ConcurrencySlotAvailable bool
}

type Admission struct {
	Allowed         bool
	Hold            HoldReason
	WeeklyPriority  bool
	CoveredEventIDs []EventID
}

// DecideAdmission is a pure preflight decision. It does not reserve, write a
// dispatch intent, or remove races with host foreground requests. The caller
// must atomically recheck/reserve/commit before invoking a guarded exact send.
func DecideAdmission(in AdmissionInput) Admission {
	deny := func(reason HoldReason) Admission { return Admission{Hold: reason} }
	if !in.Enabled {
		return deny(HoldAccountDisabled)
	}
	if in.AccountID == "" || in.Generation == "" || in.Task.AccountID != in.AccountID || in.Task.Generation != in.Generation {
		return deny(HoldCredentialChanged)
	}
	switch in.Task.State {
	case TaskDispatchIntent, TaskVerifying, TaskAwaitingEvidence:
		return deny(HoldAlreadyDispatched)
	case TaskPending, TaskWaiting, TaskReserved:
	default:
		return deny(HoldNotDue)
	}
	var due []EventID
	weekly, fiveHour, daily, manual := false, false, false, false
	seen := make(map[EventID]bool)
	for _, event := range in.Task.Intents {
		if event.ID == "" || event.Status != IntentPending || event.RandomizedDueAt.IsZero() || in.Now.Before(event.RandomizedDueAt) || seen[event.ID] {
			continue
		}
		switch event.Reason {
		case ReasonWeekly:
			weekly = true
		case ReasonFiveHour:
			fiveHour = true
		case ReasonDaily:
			daily = true
		case ReasonManual:
			manual = true
		default:
			continue
		}
		seen[event.ID] = true
		due = append(due, event.ID)
	}
	if len(due) == 0 {
		return deny(HoldNotDue)
	}
	// Any confirmed future short window blocks every policy, including weekly.
	// Weekly reconciliation must replace an old baseline using actual evidence;
	// WeeklyConfirmed alone cannot invalidate a currently active short window.
	if in.FiveHour.State == WindowActive && in.Now.Before(in.FiveHour.ResetAt) {
		if weekly && in.Weekly == WeeklyConflict {
			return deny(HoldWeeklyConflict)
		}
		return deny(HoldActiveFiveHour)
	}
	if manual && in.FiveHour.State == WindowUnknown && in.Now.Before(in.FiveHourBaselineResetAt) {
		return deny(HoldActiveFiveHour)
	}
	if in.ForegroundBusy {
		return deny(HoldForegroundBusy)
	}
	if in.Now.Before(in.BackoffUntil) {
		return deny(HoldBackoff)
	}
	if in.QuotaMaxAge <= 0 || in.QuotaObservedAt.IsZero() || in.QuotaObservedAt.After(in.Now) || in.Now.Sub(in.QuotaObservedAt) > in.QuotaMaxAge {
		return deny(HoldQuotaUnknown)
	}
	// Permission and individual window evidence have separate freshness fences.
	// A new permission/weekly observation must not revive stale short-window data.
	if in.FiveHour.ConfirmedAt.IsZero() || in.FiveHour.ConfirmedAt.After(in.Now) || in.Now.Sub(in.FiveHour.ConfirmedAt) > in.QuotaMaxAge {
		return deny(HoldQuotaUnknown)
	}
	switch in.QuotaPermission {
	case PermissionAllowed:
	case PermissionBlocked:
		return deny(HoldQuotaExhausted)
	default:
		return deny(HoldQuotaUnknown)
	}
	if in.FiveHour.State != WindowInactive && in.FiveHour.State != WindowExpired && !(manual && FullFiveHourPlaceholder(in.FiveHour)) {
		return deny(HoldQuotaUnknown)
	}
	// Expired classifications must have a past reset boundary; an inactive
	// placeholder is intentionally permitted to report a moving future boundary.
	if in.FiveHour.State == WindowExpired && (in.FiveHour.ResetAt.IsZero() || in.Now.Before(in.FiveHour.ResetAt)) {
		return deny(HoldQuotaUnknown)
	}
	if weekly && !manual {
		switch in.Weekly {
		case WeeklyConfirmed:
			if in.WeeklyObservedAt.IsZero() || in.WeeklyObservedAt.After(in.Now) || in.Now.Sub(in.WeeklyObservedAt) > in.QuotaMaxAge {
				return deny(HoldQuotaUnknown)
			}
		case WeeklyConflict:
			return deny(HoldWeeklyConflict)
		default:
			return deny(HoldWeeklyUnconfirmed)
		}
	} else if !manual {
		outsideAllowed := fiveHour && in.RecentUsers > 0 && in.AllowActiveUsersOutsideHours
		if !in.WithinAllowedHours && !outsideAllowed {
			return deny(HoldOutsideHours)
		}
		if !fiveHour && !daily {
			return deny(HoldNotDue)
		}
		if !in.OrdinarySlotReserved && in.OrdinarySlotsAvailable <= 0 {
			return deny(HoldNoCapacity)
		}
	}
	if !in.ConcurrencySlotAvailable {
		return deny(HoldNoConcurrency)
	}
	return Admission{Allowed: true, WeeklyPriority: weekly, CoveredEventIDs: due}
}

func FullFiveHourPlaceholder(window WindowEvidence) bool {
	if window.Kind != FiveHour || window.State != WindowUnknown || window.Source != "cpa_wham_usage" || window.Remaining == nil || window.Remaining.Unit != "percent" || window.Remaining.Value != 100 || window.ConfirmedAt.IsZero() {
		return false
	}
	duration := window.ResetAt.Sub(window.ConfirmedAt)
	return duration >= 5*time.Hour-time.Minute && duration <= 5*time.Hour+time.Minute
}
