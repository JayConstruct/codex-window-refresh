package runtime

import (
	"time"

	"codex-window-refresh/internal/core"
)

func expiryDue(reset, queried, now time.Time) bool {
	return !reset.IsZero() && !now.Before(reset) && !reset.Equal(queried)
}

func (e *Engine) expiryQueryDue(account core.AccountState, now time.Time) bool {
	return (e.cfg.QuotaQuery.OnFiveHourExpiry && expiryDue(account.FiveHourBaseline.StableResetAt, account.FiveHourExpiryQueried, now)) ||
		(e.cfg.QuotaQuery.OnWeeklyExpiry && expiryDue(account.WeeklyBaseline.StableResetAt, account.WeeklyExpiryQueried, now))
}

func (e *Engine) markExpiryQueried(id core.AccountID, before core.AccountState, now time.Time) {
	account, exists := e.state.Accounts[id]
	if !exists || account.Generation != before.Generation {
		return
	}
	if e.cfg.QuotaQuery.OnFiveHourExpiry && expiryDue(before.FiveHourBaseline.StableResetAt, before.FiveHourExpiryQueried, now) && !account.FiveHour.ConfirmedAt.Before(now) && account.FiveHour.AcceptedSeq > before.FiveHour.AcceptedSeq {
		account.FiveHourExpiryQueried = before.FiveHourBaseline.StableResetAt
	}
	if e.cfg.QuotaQuery.OnWeeklyExpiry && expiryDue(before.WeeklyBaseline.StableResetAt, before.WeeklyExpiryQueried, now) && !account.Weekly.ConfirmedAt.Before(now) && account.Weekly.AcceptedSeq > before.Weekly.AcceptedSeq {
		account.WeeklyExpiryQueried = before.WeeklyBaseline.StableResetAt
	}
	e.state.Accounts[id] = account
}
