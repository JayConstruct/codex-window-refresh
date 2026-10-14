package runtime

import (
	"time"

	"codex-window-refresh/internal/core"
)

type HistoryFilter struct {
	AccountID core.AccountID
	Kind      string
	Status    string
	From      time.Time
	Until     time.Time
	Cursor    uint64
	Limit     int
}

type HistoryPage struct {
	Entries        []core.HistoryEntry `json:"entries"`
	NextCursor     uint64              `json:"next_cursor"`
	RetentionDays  int                 `json:"retention_days"`
	MaximumEntries int                 `json:"maximum_entries"`
}

func (e *Engine) History(filter HistoryFilter) HistoryPage {
	page := HistoryPage{Entries: []core.HistoryEntry{}, RetentionDays: 30, MaximumEntries: maximumHistoryEntries}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 50
	}
	published := e.published.Load()
	if published == nil {
		return page
	}
	for index := len(published.state.History) - 1; index >= 0; index-- {
		entry := published.state.History[index]
		if (filter.Cursor > 0 && entry.ID >= filter.Cursor) || (filter.AccountID != "" && entry.AccountID != filter.AccountID) || (filter.Kind != "" && entry.Kind != filter.Kind) || (filter.Status != "" && entry.Status != filter.Status) || (!filter.From.IsZero() && entry.StartedAt.Before(filter.From)) || (!filter.Until.IsZero() && entry.StartedAt.After(filter.Until)) {
			continue
		}
		if len(page.Entries) == filter.Limit {
			page.NextCursor = page.Entries[len(page.Entries)-1].ID
			break
		}
		entry.Sources = append([]core.ActionSource{}, entry.Sources...)
		entry.CoveredEvents = append([]core.EventID{}, entry.CoveredEvents...)
		page.Entries = append(page.Entries, entry)
	}
	return page
}
