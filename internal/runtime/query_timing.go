package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func queryDelay(account core.AccountState, weekly, periodic bool, anchor time.Time, delay config.DelayRange) time.Duration {
	span := (delay.Max.Value() - delay.Min.Value()) / time.Second
	if span <= 0 {
		return delay.Min.Value()
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%v/%t/%t/%s", account.ID, account.Generation, weekly, periodic, anchor.UTC().Format(time.RFC3339Nano))))
	return delay.Min.Value() + time.Duration(binary.BigEndian.Uint64(digest[:8])%uint64(span+1))*time.Second
}

func (e *Engine) queryTime(account core.AccountState, weekly, periodic bool, anchor, now, until time.Time) (time.Time, bool) {
	window := e.cfg.QuotaQuery.Window(weekly)
	timing := window.Expiry
	if periodic {
		timing = window.Periodic
		anchor = e.nextRead[account.ID]
		if anchor.IsZero() {
			anchor = e.queryEpoch
		} else {
			anchor = anchor.Add(window.Interval.Value() - e.cfg.QuotaQuery.Interval.Value())
		}
	}
	due := anchor.Add(queryDelay(account, weekly, periodic, anchor, timing.RandomDelay))
	due = maxTime(due, maxTime(now, maxTime(e.nextRead[account.ID], e.nextPreflight[account.ID])))
	allowed, ok := nextQueryTime(e.cfg, timing.ExcludedHours, due, until)
	if !ok {
		return due, false
	}
	return allowed, true
}

func (e *Engine) periodicQueryDue(account core.AccountState, now time.Time) bool {
	if !e.cfg.QuotaQuery.Automatic {
		return false
	}
	for _, weekly := range []bool{false, true} {
		if !e.cfg.QuotaQuery.Window(weekly).PeriodicEnabled {
			continue
		}
		due, ok := e.queryTime(account, weekly, true, time.Time{}, now, now.Add(48*time.Hour))
		if ok && !now.Before(due) {
			return true
		}
	}
	return false
}

func nextQueryTime(settings config.Config, excluded []string, from, until time.Time) (time.Time, bool) {
	ranges, err := config.AllowedRanges(excluded)
	if err != nil || settings.Location() == nil {
		return time.Time{}, false
	}
	allowed, start := []string{}, 0
	clockText := func(minute int) string { return fmt.Sprintf("%02d:%02d", minute/60, minute%60) }
	for _, interval := range ranges {
		if start < interval.Start {
			allowed = append(allowed, clockText(start)+"-"+clockText(interval.Start))
		}
		start = interval.End
	}
	if start < 1440 {
		allowed = append(allowed, clockText(start)+"-24:00")
	}
	settings.OrdinaryRules.AllowedHours = allowed
	settings.AccountOverrides = nil
	return nextAllowedTime(settings, accountForQueryTiming, from, until)
}

const accountForQueryTiming core.AccountID = "query-timing"
