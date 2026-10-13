package runtime

import (
	"container/heap"
	"fmt"
	"sort"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

const maximumPlanEntries = 256

type PlannedAction struct {
	Kind        string         `json:"kind"`
	Trigger     string         `json:"trigger"`
	AccountID   core.AccountID `json:"account_id,omitempty"`
	RuleID      string         `json:"rule_id,omitempty"`
	EarliestAt  time.Time      `json:"earliest_at"`
	LatestAt    time.Time      `json:"latest_at"`
	RepeatEvery string         `json:"repeat_every,omitempty"`
	Status      string         `json:"status"`
	Detail      string         `json:"detail"`
	Model       string         `json:"model,omitempty"`
}

type SchedulePlan struct {
	Enabled     bool            `json:"enabled"`
	HorizonDays int             `json:"horizon_days"`
	GeneratedAt time.Time       `json:"generated_at"`
	Until       time.Time       `json:"until"`
	Entries     []PlannedAction `json:"entries"`
	Truncated   bool            `json:"truncated"`
}

type planHeap []PlannedAction

func actionBefore(first, second PlannedAction) bool {
	if !first.EarliestAt.Equal(second.EarliestAt) {
		return first.EarliestAt.Before(second.EarliestAt)
	}
	if first.Kind != second.Kind {
		return first.Kind < second.Kind
	}
	if first.Trigger != second.Trigger {
		return first.Trigger < second.Trigger
	}
	if first.AccountID != second.AccountID {
		return first.AccountID < second.AccountID
	}
	return first.RuleID < second.RuleID
}

func (entries planHeap) Len() int { return len(entries) }
func (entries planHeap) Less(first, second int) bool {
	return actionBefore(entries[second], entries[first])
}
func (entries planHeap) Swap(first, second int) {
	entries[first], entries[second] = entries[second], entries[first]
}
func (entries *planHeap) Push(value any) { *entries = append(*entries, value.(PlannedAction)) }
func (entries *planHeap) Pop() any {
	last := len(*entries) - 1
	value := (*entries)[last]
	*entries = (*entries)[:last]
	return value
}

func (e *Engine) schedulePlan(state core.CoreState, now time.Time) SchedulePlan {
	plan := SchedulePlan{Enabled: e.cfg.Planning.Enabled, HorizonDays: e.cfg.Planning.HorizonDays, GeneratedAt: now, Entries: []PlannedAction{}}
	if !plan.Enabled {
		return plan
	}
	plan.Until = now.In(e.cfg.Location()).AddDate(0, 0, plan.HorizonDays)
	entries := &planHeap{}
	add := func(action PlannedAction) {
		if action.EarliestAt.IsZero() || action.EarliestAt.After(plan.Until) {
			return
		}
		action.EarliestAt = maxTime(action.EarliestAt, now)
		if action.LatestAt.Before(action.EarliestAt) {
			action.LatestAt = action.EarliestAt
		}
		if action.LatestAt.After(plan.Until) {
			action.LatestAt = plan.Until
		}
		if entries.Len() < maximumPlanEntries {
			heap.Push(entries, action)
		} else {
			plan.Truncated = true
			if actionBefore(action, (*entries)[0]) {
				(*entries)[0] = action
				heap.Fix(entries, 0)
			}
		}
	}
	for id, account := range state.Accounts {
		policy := e.cfg.Account(id)
		if account.Task != nil && account.Task.OperationID != "" {
			add(PlannedAction{Kind: "query", Trigger: "verification", AccountID: id, EarliestAt: maxTime(account.Task.NextVerificationAt, e.nextPreflight[id]), Status: "scheduled", Detail: "核实已发送请求，不重复发送模型请求"})
			continue
		}
		if !account.Enabled || policy.Paused {
			continue
		}
		if e.cfg.QuotaQuery.Automatic {
			add(PlannedAction{Kind: "query", Trigger: "periodic", AccountID: id, EarliestAt: maxTime(now, maxTime(e.nextRead[id], e.nextPreflight[id])), RepeatEvery: e.cfg.QuotaQuery.Interval.Value().String(), Status: "estimated", Detail: "周期查询至计划结束；被动更新、退避或任务优先级可能推迟"})
		}
		for _, window := range []struct {
			reason  core.Reason
			reset   time.Time
			queried time.Time
			query   bool
			wake    bool
			delay   config.DelayRange
		}{
			{core.ReasonFiveHour, account.FiveHourBaseline.StableResetAt, account.FiveHourExpiryQueried, e.cfg.QuotaQuery.OnFiveHourExpiry, policy.FiveHourEnabled, policy.FiveHourRandomDelay},
			{core.ReasonWeekly, account.WeeklyBaseline.StableResetAt, account.WeeklyExpiryQueried, e.cfg.QuotaQuery.OnWeeklyExpiry, policy.WeeklyEnabled, policy.WeeklyRandomDelay},
		} {
			if window.reset.IsZero() {
				continue
			}
			if window.query && !window.reset.Equal(window.queried) {
				add(PlannedAction{Kind: "query", Trigger: string(window.reason), AccountID: id, EarliestAt: maxTime(window.reset, maxTime(e.nextRead[id], e.nextPreflight[id])), Status: "estimated", Detail: "已知窗口到期后查询一次；失败退避后重试"})
			}
			if !e.cfg.Enabled || !window.wake {
				continue
			}
			earliest, latest := window.reset.Add(window.delay.Min.Value()), window.reset.Add(window.delay.Max.Value())
			status := "estimated"
			if account.Task != nil {
				retired := false
				for _, intent := range account.Task.Intents {
					if intent.Reason == window.reason && intent.OriginalDueAt.Equal(window.reset) {
						if intent.Status != core.IntentPending {
							retired = true
						} else {
							earliest, latest, status = intent.RandomizedDueAt, intent.RandomizedDueAt, "scheduled"
						}
					}
				}
				if retired {
					continue
				}
			}
			earliest = maxTime(earliest, maxTime(now, maxTime(account.BackoffUntil, e.nextPreflight[id])))
			detail := "需发送前额度核对；已有活跃窗口或待验证请求不会发送"
			if window.reason == core.ReasonFiveHour {
				allowed, ok := nextAllowedTime(e.cfg, id, earliest, plan.Until)
				if !ok {
					status, detail = "blocked", "计划范围内没有允许的五小时唤醒时段"
				} else {
					earliest = allowed
				}
				detail += "；仍需人数容量满足"
			}
			model, _ := e.cfg.WakeContent(window.reason)
			add(PlannedAction{Kind: "wake", Trigger: string(window.reason), AccountID: id, EarliestAt: earliest, LatestAt: latest, Status: status, Detail: detail, Model: model})
		}
	}
	if e.cfg.Enabled && e.cfg.OrdinaryRules.DailyEnabled {
		startDate := now.In(e.cfg.Location())
		for offset := 0; offset <= plan.HorizonDays; offset++ {
			date := startDate.AddDate(0, 0, offset).Format("2006-01-02")
			for _, rule := range e.cfg.DailyRules {
				if !rule.Enabled {
					continue
				}
				rangeValue, err := rule.MinuteRange()
				if err != nil {
					continue
				}
				earliest, err := resolveDailyWallTime(date, time.Duration(rangeValue.Start)*time.Minute, e.cfg.Location())
				if err != nil {
					continue
				}
				latest, err := resolveDailyWallTime(date, time.Duration(rangeValue.End)*time.Minute, e.cfg.Location())
				if err != nil {
					continue
				}
				if rangeValue.Start != rangeValue.End && latest.After(earliest) {
					latest = latest.Add(-time.Nanosecond)
				}
				status, retired := "estimated", false
				for _, intent := range state.DailyIntents {
					if intent.RuleID == rule.ID && intent.CycleID == date {
						if intent.Status != core.IntentPending {
							retired = true
						} else {
							earliest, latest, status = intent.RandomizedDueAt, intent.RandomizedDueAt, "scheduled"
						}
					}
				}
				if retired {
					continue
				}
				model, _ := e.cfg.WakeContent(core.ReasonDaily)
				add(PlannedAction{Kind: "wake", Trigger: string(core.ReasonDaily), RuleID: rule.ID, EarliestAt: earliest, LatestAt: latest, Status: status, Detail: fmt.Sprintf("按容量选择账号，最低就绪 %d；仍受五小时允许时段及额度限制", rule.MinimumReadyAccounts), Model: model})
			}
		}
	}
	plan.Entries = append(plan.Entries, (*entries)...)
	sort.Slice(plan.Entries, func(first, second int) bool { return actionBefore(plan.Entries[first], plan.Entries[second]) })
	return plan
}

func nextAllowedTime(settings config.Config, id core.AccountID, from, until time.Time) (time.Time, bool) {
	ranges, err := config.AllowedRanges(settings.Account(id).AllowedHours)
	if err != nil || len(ranges) == 0 {
		return time.Time{}, false
	}
	if from.After(until) {
		return time.Time{}, false
	}
	if settings.WithinAllowedHours(from, id) {
		return from, true
	}
	_, currentOffset := from.In(settings.Location()).Zone()
	_, laterOffset := from.Add(3 * time.Hour).In(settings.Location()).Zone()
	if laterOffset < currentOffset {
		for minute := 1; minute <= 180; minute++ {
			candidate := from.Truncate(time.Minute).Add(time.Duration(minute) * time.Minute)
			if candidate.After(until) {
				break
			}
			if settings.WithinAllowedHours(candidate, id) {
				return candidate, true
			}
		}
	}
	date := from.In(settings.Location())
	for offset := 0; offset <= 31; offset++ {
		day := date.AddDate(0, 0, offset).Format("2006-01-02")
		for _, rangeValue := range ranges {
			start, startErr := resolveDailyWallTime(day, time.Duration(rangeValue.Start)*time.Minute, settings.Location())
			if startErr != nil {
				continue
			}
			candidate := maxTime(start, from)
			local := candidate.In(settings.Location())
			minute := local.Hour()*60 + local.Minute()
			if local.Format("2006-01-02") == day && minute < rangeValue.Start {
				candidate = candidate.Truncate(time.Minute).Add(time.Duration(rangeValue.Start-minute) * time.Minute)
				local = candidate.In(settings.Location())
				minute = local.Hour()*60 + local.Minute()
			}
			if local.Format("2006-01-02") == day && minute >= rangeValue.Start && minute < rangeValue.End && !candidate.After(until) {
				return candidate, true
			}
		}
		if date.AddDate(0, 0, offset).After(until) {
			break
		}
	}
	return time.Time{}, false
}
