package runtime

import (
	"context"
	"sort"
	"time"
)

type ManualQueryStatus struct {
	Phase      string    `json:"phase"`
	Total      int       `json:"total"`
	Completed  int       `json:"completed"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// RequestQuotaQuery queues one coalesced query-only sweep. The HTTP callback
// never performs network I/O or calls the scheduler's model dispatch path.
func (e *Engine) RequestQuotaQuery() bool {
	e.queueMu.Lock()
	if e.manualActive {
		e.queueMu.Unlock()
		return false
	}
	e.manualRequested, e.manualActive = true, true
	e.queueMu.Unlock()
	e.Notify()
	return true
}
func (e *Engine) startManualQuery() {
	e.queueMu.Lock()
	requested := e.manualRequested
	e.manualRequested = false
	e.queueMu.Unlock()
	if requested {
		e.manualQuery = ManualQueryStatus{Phase: "queued", StartedAt: e.clock.Now()}
		e.manualIDs = nil
		e.manualIndex = 0
		// Synchronize inventory once for this sweep, including newly added accounts.
		e.nextInventory = time.Time{}
	}
}
func (e *Engine) finishManualQuery(phase string) {
	e.manualQuery.Phase, e.manualQuery.FinishedAt = phase, e.clock.Now()
	e.manualIDs = nil
	e.queueMu.Lock()
	e.manualActive = false
	e.queueMu.Unlock()
}
func (e *Engine) stepManualQuery(ctx context.Context) {
	if !e.cfg.QuotaQuery.Manual {
		e.finishManualQuery("cancelled")
		return
	}
	if !e.caps.QuotaAdapterReady {
		e.finishManualQuery("unavailable")
		return
	}
	if e.manualQuery.Phase == "queued" {
		for _, a := range e.inventory {
			if a.Enabled {
				e.manualIDs = append(e.manualIDs, a.ID)
			}
		}
		sort.Slice(e.manualIDs, func(i, j int) bool { return e.manualIDs[i] < e.manualIDs[j] })
		e.manualQuery.Total, e.manualQuery.Phase = len(e.manualIDs), "running"
	}
	// Query all enabled host accounts, including ones paused only for model
	// wake. Bound each cycle; cancellation and failed queries never retry a key.
	for n := 0; n < 4 && e.manualIndex < len(e.manualIDs) && ctx.Err() == nil; n++ {
		id := e.manualIDs[e.manualIndex]
		e.manualIndex++
		available := false
		for _, a := range e.inventory {
			if a.ID == id {
				available = a.Enabled
				break
			}
		}
		if !available {
			e.manualQuery.Skipped++
			continue
		}
		if err := e.observe(ctx, id); err != nil {
			e.manualQuery.Failed++
		} else {
			e.manualQuery.Completed++
		}
	}
	if e.manualIndex >= len(e.manualIDs) {
		e.finishManualQuery("completed")
	}
}
