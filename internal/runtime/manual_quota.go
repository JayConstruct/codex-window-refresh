package runtime

import (
	"context"
	"sort"
	"time"

	"codex-window-refresh/internal/core"
)

type ManualQueryStatus struct {
	JobID         string                `json:"job_id,omitempty"`
	BlockedReason string                `json:"blocked_reason,omitempty"`
	RetryAt       time.Time             `json:"retry_at,omitempty"`
	AccountID     core.AccountID        `json:"account_id,omitempty"`
	Phase         string                `json:"phase"`
	Total         int                   `json:"total"`
	Completed     int                   `json:"completed"`
	Failed        int                   `json:"failed"`
	Skipped       int                   `json:"skipped"`
	LastError     string                `json:"last_error,omitempty"`
	Diagnostic    *core.QuotaDiagnostic `json:"diagnostic,omitempty"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at"`
}

// RequestQuotaQuery queues one coalesced query-only sweep. The HTTP callback
// never performs network I/O or calls the scheduler's model dispatch path.
func (e *Engine) RequestQuotaQuery() bool {
	return e.QueueQuotaQuery("") == "queued"
}

// QueueQuotaQuery selects exactly one account, or a sweep when id is empty.
// Different targets cannot silently join or widen a running query.
func (e *Engine) QueueQuotaQuery(id core.AccountID) string {
	status, _ := e.QueueQuotaQueryJob(id)
	return status
}

// QueueQuotaQueryJob returns the accepted/coalesced identity under the same lock.
func (e *Engine) QueueQuotaQueryJob(id core.AccountID) (string, string) {
	e.queueMu.Lock()
	publishQueued := false
	defer func() {
		e.queueMu.Unlock()
		if publishQueued {
			e.publishQueuedJobs()
		}
	}()
	if e.manualWakeActive || e.generationActive {
		return "busy", ""
	}
	if e.manualActive {
		if e.manualTarget == id {
			publishQueued = true
			return "already_queued", e.manualJobID
		}
		return "busy", ""
	}
	if e.queuePoisoned || e.lastPublishedCommitFailed() {
		return "durable_commit_failed", ""
	}
	e.manualRequested, e.manualActive = true, true
	e.manualTarget, e.manualJobID = id, newJobID()
	queued := ManualQueryStatus{JobID: e.manualJobID, AccountID: id, Phase: "queued", StartedAt: e.clock.Now()}
	e.putJobLocked(JobStatus{JobID: e.manualJobID, Kind: "quota_query", Phase: "queued", AccountID: id, QuotaQuery: &queued})
	publishQueued = true
	e.Notify()
	return "queued", e.manualJobID
}

func (e *Engine) startManualQuery() {
	e.queueMu.Lock()
	requested := e.manualRequested
	target, jobID := e.manualTarget, e.manualJobID
	e.manualRequested = false
	e.queueMu.Unlock()
	if requested {
		e.manualQuery = ManualQueryStatus{JobID: jobID, AccountID: target, Phase: "queued", StartedAt: e.clock.Now()}
		e.manualIDs = nil
		e.manualIndex = 0
		// Synchronize inventory once for this sweep, including newly added accounts.
		e.nextInventory = time.Time{}
	}
}
func (e *Engine) finishManualQuery(phase string) {
	e.manualQuery.Phase, e.manualQuery.FinishedAt = phase, e.clock.Now()
	e.manualQuery.BlockedReason, e.manualQuery.RetryAt = "", time.Time{}
	e.manualIDs = nil
	e.recordQuotaJob()
	e.queueMu.Lock()
	e.manualActive = false
	e.queueMu.Unlock()
}
func (e *Engine) stepManualQuery(ctx context.Context) {
	if !e.caps.QuotaAdapterReady {
		e.finishManualQuery("unavailable")
		return
	}
	if e.manualQuery.Phase == "queued" {
		if e.manualQuery.AccountID != "" {
			e.manualIDs = append(e.manualIDs, e.manualQuery.AccountID)
		} else {
			for _, a := range e.inventory {
				e.manualIDs = append(e.manualIDs, a.ID)
			}
		}
		sort.Slice(e.manualIDs, func(i, j int) bool { return e.manualIDs[i] < e.manualIDs[j] })
		e.manualQuery.Total, e.manualQuery.Phase = len(e.manualIDs), "running"
	}
	e.recordQuotaJob()
	e.publish(false)
	for n := 0; n < 4 && e.manualIndex < len(e.manualIDs) && ctx.Err() == nil; n++ {
		id := e.manualIDs[e.manualIndex]
		e.manualIndex++
		available := false
		for _, a := range e.inventory {
			if a.ID == id {
				available = true
				break
			}
		}
		if !available {
			e.manualQuery.Skipped++
			core.AppendHistory(&e.state, core.HistoryEntry{Kind: "query", AccountID: id, StartedAt: e.clock.Now(), FinishedAt: e.clock.Now(), Status: "skipped", ErrorCode: "account_removed", Sources: []core.ActionSource{{Trigger: "manual_query"}}})
			continue
		}
		if err := e.observe(ctx, id, core.ActionSource{Trigger: "manual_query"}); err != nil {
			e.manualQuery.Failed++
			e.manualQuery.LastError = quotaReadErrorCode(err)
			diagnostic := quotaReadDiagnostic(err, 0)
			if failure, ok := err.(*quotaReadFailure); ok {
				diagnostic = failure.diagnostic
			}
			e.manualQuery.Diagnostic = &diagnostic
		} else {
			e.manualQuery.Completed++
		}
		e.recordQuotaJob()
		e.publish(false)
	}
	if e.manualIndex >= len(e.manualIDs) {
		e.finishManualQuery("completed")
	}
}
