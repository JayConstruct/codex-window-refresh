package runtime

import (
	"time"

	"codex-window-refresh/internal/core"
)

func expiryDue(reset, queried, now time.Time) bool {
	return !reset.IsZero() && !now.Before(reset) && !reset.Equal(queried)
}

func (e *Engine) expiryQueryDue(account core.AccountState, now time.Time) bool {
	return e.windowExpiryQueryDue(account, false, now) || e.windowExpiryQueryDue(account, true, now)
}

func (e *Engine) windowExpiryQueryDue(account core.AccountState, weekly bool, now time.Time) bool {
	reset, queried, enabled := account.FiveHourBaseline.StableResetAt, account.FiveHourExpiryQueried, e.cfg.QuotaQuery.OnFiveHourExpiry
	if weekly {
		reset, queried, enabled = account.WeeklyBaseline.StableResetAt, account.WeeklyExpiryQueried, e.cfg.QuotaQuery.OnWeeklyExpiry
	}
	if !enabled || !expiryDue(reset, queried, now) {
		return false
	}
	timing := e.cfg.QuotaQuery.Window(weekly).Expiry
	due := reset.Add(queryDelay(account, weekly, false, reset, timing.RandomDelay))
	due, ok := nextQueryTime(e.cfg, timing.ExcludedHours, maxTime(now, due), maxTime(now, due).Add(48*time.Hour))
	return ok && !now.Before(due)
}

func (e *Engine) markExpiryQueried(id core.AccountID, before core.AccountState, now time.Time) {
	account, exists := e.state.Accounts[id]
	if !exists || account.Generation != before.Generation {
		return
	}
	if e.windowExpiryQueryDue(before, false, now) && !account.FiveHour.ConfirmedAt.Before(now) && account.FiveHour.AcceptedSeq > before.FiveHour.AcceptedSeq {
		account.FiveHourExpiryQueried = before.FiveHourBaseline.StableResetAt
	}
	if e.windowExpiryQueryDue(before, true, now) && !account.Weekly.ConfirmedAt.Before(now) && account.Weekly.AcceptedSeq > before.Weekly.AcceptedSeq {
		account.WeeklyExpiryQueried = before.WeeklyBaseline.StableResetAt
	}
	e.state.Accounts[id] = account
}
