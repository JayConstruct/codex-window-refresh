package runtime

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

const dailyHistoryPerRule = 8

// reconcileDaily is a pure state transition: the caller persists its result
// before dispatch. Dates not recorded during downtime are never reconstructed.
// Previously persisted pending dates survive restart and may catch up.
func reconcileDaily(s *core.CoreState, cfg config.Config, now time.Time, rng core.Random) error {
	if s == nil {
		return errors.New("runtime: nil daily state")
	}
	loc := cfg.Location()
	if loc == nil {
		return errors.New("runtime: invalid daily timezone")
	}
	today := now.In(loc).Format("2006-01-02")
	rules := make(map[string]config.DailyRule, len(cfg.DailyRules))
	ranges := make(map[string]config.MinuteRange, len(cfg.DailyRules))
	for _, rule := range cfg.DailyRules {
		if _, exists := rules[rule.ID]; exists {
			return errors.New("runtime: duplicate daily rule ID")
		}
		rules[rule.ID] = rule
		if !rule.Enabled {
			continue
		}
		parsed, err := rule.MinuteRange()
		if err != nil {
			return fmt.Errorf("runtime: invalid daily time range for %s: %w", rule.ID, err)
		}
		ranges[rule.ID] = parsed
	}
	// Work on independent values; a failing sample or civil-time resolution must
	// leave the caller's state unchanged.
	next := append([]core.PoolDailyIntent(nil), s.DailyIntents...)
	seen := make(map[string]bool, len(next))
	latestDate := make(map[string]string, len(cfg.DailyRules))
	for i := range next {
		intent := &next[i]
		if intent.Reason != core.ReasonDaily {
			continue
		}
		seen[dailyKey(intent.RuleID, intent.CycleID)] = true
		if intent.CycleID > latestDate[intent.RuleID] {
			latestDate[intent.RuleID] = intent.CycleID
		}
		if intent.Status != core.IntentPending {
			continue
		}
		rule, exists := rules[intent.RuleID]
		if !exists || !rule.Enabled {
			intent.Status = core.IntentSuperseded
			continue
		}
		intent.MinimumReadyAccounts = rule.MinimumReadyAccounts
		if !cfg.Execution.CatchUp || !rule.CatchUp {
			expiry, err := dailyExpiry(intent.CycleID, ranges[rule.ID], intent.RandomizedDueAt, loc)
			if err != nil {
				return err
			}
			if !now.Before(expiry) {
				intent.Status = core.IntentSuperseded
			}
		}
	}
	if cfg.Enabled {
		for _, rule := range cfg.DailyRules {
			// Retained history also acts as a date watermark: a wall-clock
			// rollback must not recreate an older, already-pruned daily event.
			if !rule.Enabled || seen[dailyKey(rule.ID, today)] || today < latestDate[rule.ID] {
				continue
			}
			r := ranges[rule.ID]
			start := time.Duration(r.Start) * time.Minute
			offset := start
			if r.End > r.Start {
				maximum := time.Duration(r.End-r.Start)*time.Minute - time.Nanosecond
				if rng == nil {
					return errors.New("runtime: missing daily random source")
				}
				sampled, err := rng.Duration(0, maximum)
				if err != nil {
					return fmt.Errorf("runtime: daily random sample: %w", err)
				}
				if sampled < 0 || sampled > maximum {
					return errors.New("runtime: daily random sample outside bounds")
				}
				offset += sampled
			}
			original, err := resolveDailyWallTime(today, start, loc)
			if err != nil {
				return err
			}
			due, err := resolveDailyWallTime(today, offset, loc)
			if err != nil {
				return err
			}
			intent := core.PoolDailyIntent{Intent: core.Intent{
				ID: core.EventID("daily:" + rule.ID + ":" + today), Reason: core.ReasonDaily,
				CycleID: today, RuleID: rule.ID, OriginalDueAt: original,
				RandomizedDueAt: due, CreatedAt: now, Status: core.IntentPending,
			}, MinimumReadyAccounts: rule.MinimumReadyAccounts}
			if !cfg.Execution.CatchUp || !rule.CatchUp {
				expiry, err := dailyExpiry(today, r, due, loc)
				if err != nil {
					return err
				}
				if !now.Before(expiry) {
					intent.Status = core.IntentSuperseded
				}
			}
			next = append(next, intent)
		}
	}
	s.DailyIntents = pruneDailyHistory(next)
	return nil
}

func dailyKey(ruleID, date string) string { return ruleID + ":" + date }

func dailyExpiry(date string, r config.MinuteRange, due time.Time, loc *time.Location) (time.Time, error) {
	end, err := resolveDailyWallTime(date, time.Duration(r.End)*time.Minute, loc)
	if err != nil {
		return time.Time{}, err
	}
	// An exact schedule has a minute in which a polling worker can execute it.
	// A nonexistent DST range maps to the first valid minute, which must also
	// remain eligible for that minute instead of expiring before dispatch.
	if r.Start == r.End || !end.After(due) {
		end = due.Add(time.Minute)
	}
	return end, nil
}

// resolveDailyWallTime chooses the earliest real occurrence of an ambiguous
// local time. A nonexistent wall time advances to the first valid minute at or
// after it, rather than accepting time.Date's unspecified DST normalization.
func resolveDailyWallTime(date string, offset time.Duration, loc *time.Location) (time.Time, error) {
	civil, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, errors.New("runtime: invalid persisted daily date")
	}
	desired := civil.Add(offset)
	offsets := make(map[int]struct{})
	// IANA civil transitions, including a whole skipped date, fit within this
	// neighborhood. Sampling real instants obtains both sides' actual offsets.
	for h := -72; h <= 72; h++ {
		_, zoneOffset := desired.Add(time.Duration(h) * time.Hour).In(loc).Zone()
		offsets[zoneOffset] = struct{}{}
	}
	find := func(wall time.Time) (time.Time, bool) {
		var earliest time.Time
		for zoneOffset := range offsets {
			candidate := wall.Add(-time.Duration(zoneOffset) * time.Second)
			local := candidate.In(loc)
			if local.Year() != wall.Year() || local.Month() != wall.Month() || local.Day() != wall.Day() || local.Hour() != wall.Hour() || local.Minute() != wall.Minute() || local.Second() != wall.Second() || local.Nanosecond() != wall.Nanosecond() {
				continue
			}
			if earliest.IsZero() || candidate.Before(earliest) {
				earliest = candidate
			}
		}
		return earliest, !earliest.IsZero()
	}
	if instant, ok := find(desired); ok {
		return instant.In(loc), nil
	}
	minute := desired.Truncate(time.Minute)
	if minute.Before(desired) {
		minute = minute.Add(time.Minute)
	}
	for i := 0; i < 3*24*60; i++ {
		if instant, ok := find(minute); ok {
			return instant.In(loc), nil
		}
		minute = minute.Add(time.Minute)
	}
	return time.Time{}, errors.New("runtime: cannot resolve daily civil time")
}

func pruneDailyHistory(intents []core.PoolDailyIntent) []core.PoolDailyIntent {
	groups := make(map[string][]core.PoolDailyIntent)
	result := make([]core.PoolDailyIntent, 0, len(intents))
	for _, intent := range intents {
		if intent.Reason != core.ReasonDaily {
			result = append(result, intent)
			continue
		}
		groups[intent.RuleID] = append(groups[intent.RuleID], intent)
	}
	for _, history := range groups {
		sort.SliceStable(history, func(i, j int) bool {
			if history[i].CycleID != history[j].CycleID {
				return history[i].CycleID > history[j].CycleID
			}
			return history[i].CreatedAt.After(history[j].CreatedAt)
		})
		newestPending := -1
		for i, intent := range history {
			if intent.Status == core.IntentPending {
				newestPending = i
				break
			}
		}
		if len(history) > dailyHistoryPerRule {
			if newestPending >= dailyHistoryPerRule {
				retained := history[newestPending]
				history = history[:dailyHistoryPerRule]
				history[dailyHistoryPerRule-1] = retained
			} else {
				history = history[:dailyHistoryPerRule]
			}
		}
		result = append(result, history...)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if !result[i].OriginalDueAt.Equal(result[j].OriginalDueAt) {
			return result[i].OriginalDueAt.Before(result[j].OriginalDueAt)
		}
		if result[i].RuleID != result[j].RuleID {
			return result[i].RuleID < result[j].RuleID
		}
		return result[i].ID < result[j].ID
	})
	return result
}
