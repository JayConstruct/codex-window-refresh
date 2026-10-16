package runtime

import (
	"strings"
	"time"

	"codex-window-refresh/internal/core"
)

const historyRetention = 30 * 24 * time.Hour
const maximumHistoryEntries = 10000

func (e *Engine) recentUsageAllowed(state core.CoreState, now time.Time) bool {
	condition := e.cfg.QuotaQuery.FiveHourRecentUsage
	return !condition.Enabled || core.ActiveUserCount(state, now, condition.Lookback.Value()) > 0
}

func (e *Engine) reconcileHistory(now time.Time) {
	entries := e.state.History[:0]
	for _, entry := range e.state.History {
		if entry.Provider != "antigravity" && entry.OperationID != "" && (entry.Status == "dispatched" || entry.Status == "verifying" || entry.Status == "unknown") {
			account, exists := e.state.Accounts[entry.AccountID]
			if !exists || account.Generation != entry.Generation {
				entry.Status, entry.ErrorCode = "unknown", "account_binding_changed"
			}
			if exists && account.Generation == entry.Generation && account.Task != nil {
				verified := len(entry.CoveredEvents) > 0
				for _, covered := range entry.CoveredEvents {
					found := false
					for _, intent := range account.Task.Intents {
						if intent.ID == covered && intent.Status == core.IntentSatisfied {
							found = true
						}
					}
					verified = verified && found
				}
				if verified && entry.Kind == "wake" {
					entry.Status, entry.FinishedAt = "verified", now
				}
			}
		}
		if !entry.StartedAt.Before(now.Add(-historyRetention)) {
			entries = append(entries, entry)
		}
	}
	if len(entries) > maximumHistoryEntries {
		copy(entries, entries[len(entries)-maximumHistoryEntries:])
		entries = entries[:maximumHistoryEntries]
	}
	// Pruned prompts and source slices must not remain reachable in the unused
	// part of the backing array, including after history reaches its entry limit.
	clear(e.state.History[len(entries):])
	e.state.History = entries
}

func queryEvidenceSource(account core.AccountState, weekly bool, trigger, path string) core.ActionSource {
	evidence, baseline := account.FiveHour, account.FiveHourBaseline
	if weekly {
		evidence, baseline = account.Weekly, account.WeeklyBaseline
	}
	return core.ActionSource{Trigger: trigger, ConfigPath: path, EvidenceSource: evidence.Source, EvidenceAt: evidence.ConfirmedAt, ResetAt: baseline.StableResetAt, CycleID: baseline.CycleID}
}

func wakeSources(account core.AccountState, covered []core.EventID) []core.ActionSource {
	sources := []core.ActionSource{}
	if account.Task == nil {
		return sources
	}
	for _, intent := range account.Task.Intents {
		for _, id := range covered {
			if intent.ID != id {
				continue
			}
			path := "reset_rules.five_hour"
			if intent.Reason == core.ReasonWeekly {
				path = "reset_rules.weekly"
			}
			if intent.Reason == core.ReasonDaily {
				path = "daily_rules"
			}
			if intent.Reason == core.ReasonManual {
				path = "manual_wake"
			}
			if intent.Reason == core.ReasonGeneration {
				path = "prompt_generation"
			}
			source := queryEvidenceSource(account, intent.Reason == core.ReasonWeekly, string(intent.Reason), path)
			source.EventID, source.RuleID = intent.ID, intent.RuleID
			sources = append(sources, source)
		}
	}
	return sources
}

func (e *Engine) automaticQuerySources(account core.AccountState, now time.Time, verifying bool) []core.ActionSource {
	sources := []core.ActionSource{}
	if verifying {
		source := queryEvidenceSource(account, false, "verification", "execution.verify_delays")
		if account.Task != nil && len(account.Task.CoveredEventIDs) > 0 {
			source.EventID = account.Task.CoveredEventIDs[0]
		}
		sources = append(sources, source)
		if account.Task != nil {
			sources = append(sources, wakeSources(account, account.Task.CoveredEventIDs)...)
		}
		return sources
	}
	for _, weekly := range []bool{false, true} {
		window, reason := "five_hour", core.ReasonFiveHour
		if weekly {
			window, reason = "weekly", core.ReasonWeekly
		}
		if e.windowExpiryQueryDue(account, weekly, now) {
			sources = append(sources, queryEvidenceSource(account, weekly, string(reason), "quota_query."+window+".expiry"))
		}
		if e.cfg.QuotaQuery.Automatic && e.cfg.QuotaQuery.Window(weekly).PeriodicEnabled {
			due, ok := e.queryTime(account, weekly, true, time.Time{}, now, now.Add(48*time.Hour))
			if ok && !now.Before(due) {
				sources = append(sources, queryEvidenceSource(account, weekly, "periodic", "quota_query."+window+".periodic"))
			}
		}
	}
	return sources
}

func boundedHistoryPrompt(prompt string) string {
	if len(prompt) <= 4096 {
		return prompt
	}
	return strings.ToValidUTF8(prompt[:4096], "")
}
