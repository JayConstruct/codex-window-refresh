package runtime

import (
	"context"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
)

// publishedStatus is immutable once stored. Network operations never hold the
// reader's lock, and an unsuccessful commit cannot publish uncommitted state.
type publishedStatus struct {
	state     core.CoreState
	caps      core.HostCapabilities
	lastError string
	scheduler SchedulingSummary
}
type SchedulingSummary struct {
	ObservedAt             time.Time             `json:"observed_at"`
	RecentUsers            int                   `json:"recent_users"`
	IdentitySource         string                `json:"identity_source"`
	Capacity               core.Capacity         `json:"capacity"`
	HoldCounts             map[string]int        `json:"hold_counts"`
	QuotaUnknownAccounts   int                   `json:"quota_unknown_accounts"`
	ForegroundBusyAccounts int                   `json:"foreground_busy_accounts"`
	NextInventoryAt        time.Time             `json:"next_inventory_at"`
	QuotaQuery             ManualQueryStatus     `json:"quota_query"`
	ManualWake             ManualWakeStatus      `json:"manual_wake"`
	Plan                   SchedulePlan          `json:"plan"`
	LastQuotaDiagnostic    *core.QuotaDiagnostic `json:"last_quota_diagnostic,omitempty"`
}

func (e *Engine) publish(committed bool) {
	previous := e.published.Load()
	var state core.CoreState
	if previous == nil || committed {
		state = cloneState(e.state)
	} else {
		state = previous.state
	}
	now := e.clock.Now()
	summary := SchedulingSummary{ObservedAt: now, RecentUsers: core.ActiveUserCount(state, now, e.cfg.Activity.Lookback.Value()), IdentitySource: e.cfg.Activity.IdentitySource, Capacity: capacityFor(e.cfg, state, now, ""), HoldCounts: map[string]int{}, NextInventoryAt: e.nextInventory}
	summary.QuotaQuery = e.manualQuery
	summary.Plan = e.schedulePlan(state, now)
	summary.ManualWake = e.manualWake
	if summary.ManualWake.Phase == "verifying" {
		account := state.Accounts[summary.ManualWake.AccountID]
		if account.Task != nil {
			for _, event := range account.Task.Intents {
				if event.ID == summary.ManualWake.EventID && event.Status == core.IntentSatisfied {
					summary.ManualWake.Phase = "completed"
				}
			}
		}
	}
	summary.LastQuotaDiagnostic = e.lastQuotaDiagnostic
	busyAccounts := map[core.AccountID]bool{}
	for _, r := range state.Requests {
		if !r.Completed && r.AccountID != "" && state.Accounts[r.AccountID].Generation == r.Generation {
			busyAccounts[r.AccountID] = true
		}
	}
	for id, a := range state.Accounts {
		busy := busyAccounts[id]
		unknown := a.Permission == core.PermissionUnknown || a.FiveHour.State == core.WindowUnknown || a.FiveHour.State == ""
		if busy {
			summary.ForegroundBusyAccounts++
		}
		if unknown {
			summary.QuotaUnknownAccounts++
		}
		reason := ""
		switch {
		case !e.cfg.Enabled:
			reason = "plugin_disabled"
		case e.inventoryFailed:
			reason = "account_inventory_unavailable"
		case !a.Enabled || e.cfg.Account(id).Paused:
			reason = string(core.HoldAccountDisabled)
		case a.Task != nil && a.Task.OperationID != "":
			reason = string(core.HoldAlreadyDispatched)
		case !e.caps.QuotaAdapterReady:
			reason = "quota_adapter_unavailable"
		case busy:
			reason = string(core.HoldForegroundBusy)
		case now.Before(a.BackoffUntil):
			reason = string(core.HoldBackoff)
		case a.FiveHour.State == core.WindowActive && now.Before(a.FiveHour.ResetAt):
			reason = string(core.HoldActiveFiveHour)
		case a.Permission == core.PermissionBlocked:
			reason = string(core.HoldQuotaExhausted)
		case unknown:
			reason = string(core.HoldQuotaUnknown)
		case a.Task != nil && a.Task.HoldReason != "":
			reason = string(a.Task.HoldReason)
		case a.Task != nil && oldestDue(a, now).IsZero():
			reason = string(core.HoldNotDue)
		}
		if reason != "" {
			summary.HoldCounts[reason]++
		}
	}
	e.published.Store(&publishedStatus{state: state, caps: e.caps, lastError: e.lastError, scheduler: summary})
}
func (e *Engine) SchedulerDiagnostics() any {
	v := e.published.Load()
	if v == nil {
		return SchedulingSummary{HoldCounts: map[string]int{}}
	}
	s := v.scheduler
	s.Plan.Entries = append([]PlannedAction{}, s.Plan.Entries...)
	if s.LastQuotaDiagnostic != nil {
		d := *s.LastQuotaDiagnostic
		s.LastQuotaDiagnostic = &d
	}
	if s.QuotaQuery.Diagnostic != nil {
		d := *s.QuotaQuery.Diagnostic
		s.QuotaQuery.Diagnostic = &d
	}
	s.HoldCounts = make(map[string]int, len(v.scheduler.HoldCounts))
	for k, n := range v.scheduler.HoldCounts {
		s.HoldCounts[k] = n
	}
	return s
}
func (e *Engine) readFailed(id core.AccountID) {
	n := e.readFailures[id] + 1
	e.readFailures[id] = min(n, 5)
	delay := min(time.Duration(1<<min(n-1, 4))*30*time.Second, 5*time.Minute)
	e.nextRead[id] = e.clock.Now().Add(delay)
	e.nextPreflight[id] = e.nextRead[id]
}

// Only fixed public classifications may reach diagnostics; host errors can
// contain tokens, proxy credentials, or upstream response bodies.
func quotaReadErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "quota_query_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "quota_query_cancelled"
	}
	switch code := err.Error(); code {
	case "quota_credential_missing", "quota_credential_expired", "quota_proxy_invalid", "quota_host_http_failed", "quota_native_query_failed",
		"quota_host_operation_failed", "quota_query_timeout", "quota_query_cancelled", "quota_upstream_forbidden", "quota_upstream_rate_limited", "quota_upstream_server_failed", "quota_upstream_http_failed", "quota_response_invalid", "quota_response_too_large", "quota_response_unrecognized", "quota_management_auth_failed", "quota_management_endpoint_missing", "quota_management_http_failed", "quota_management_bridge_failed", "quota_management_bridge_missing", "account_identity_unavailable", "account_binding_changed":
		return code
	default:
		return "quota_read_failed"
	}
}

type quotaReadFailure struct{ diagnostic core.QuotaDiagnostic }

func (e *quotaReadFailure) Error() string                         { return e.diagnostic.Code }
func (e *quotaReadFailure) QuotaDiagnostic() core.QuotaDiagnostic { return e.diagnostic }

func quotaReadDiagnostic(err error, elapsed time.Duration) core.QuotaDiagnostic {
	d := core.QuotaDiagnostic{Code: quotaReadErrorCode(err), Stage: "query", ElapsedMS: max(0, elapsed.Milliseconds())}
	var provider interface{ QuotaDiagnostic() core.QuotaDiagnostic }
	if errors.As(err, &provider) {
		candidate := provider.QuotaDiagnostic()
		switch candidate.Stage {
		case "credential", "proxy", "operation_open", "operation_cancel", "host_http", "proxy_http", "management_config", "management_http", "management_parse", "upstream_http", "parse", "binding", "query":
			d.Stage = candidate.Stage
		}
		if candidate.HTTPStatus >= 100 && candidate.HTTPStatus <= 599 {
			d.HTTPStatus = candidate.HTTPStatus
		}
	}
	return d
}
