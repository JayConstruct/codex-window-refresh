package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"codex-window-refresh/internal/core"
)

const retainedJobLimit = 64
const manualJobBlockLimit = 2 * time.Minute

// JobStatus identifies a single accepted operation, including results displaced
// by subsequent operations from another browser. Payloads are immutable copies.
type JobStatus struct {
	pendingCommit    bool
	JobID            string                    `json:"job_id"`
	Kind             string                    `json:"kind"`
	Phase            string                    `json:"phase"`
	AccountID        core.AccountID            `json:"account_id,omitempty"`
	UpdatedAt        time.Time                 `json:"updated_at"`
	BlockedReason    string                    `json:"blocked_reason,omitempty"`
	RetryAt          time.Time                 `json:"retry_at,omitempty"`
	QuotaQuery       *ManualQueryStatus        `json:"quota_query,omitempty"`
	ManualWake       *ManualWakeStatus         `json:"manual_wake,omitempty"`
	PromptGeneration *core.PromptGenerationJob `json:"prompt_generation,omitempty"`
}

func newJobID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		// crypto/rand.Read cannot fail on supported Go systems. Refuse to
		// acknowledge an operation without a unique identity if it does.
		panic("runtime job identity unavailable")
	}
	return hex.EncodeToString(bytes[:])
}

func (e *Engine) lastPublishedCommitFailed() bool {
	v := e.published.Load()
	return v != nil && v.lastError == "durable_commit_failed"
}

func cloneJobStatuses(jobs []JobStatus) []JobStatus {
	result := []JobStatus{}
	data, _ := json.Marshal(jobs)
	_ = json.Unmarshal(data, &result)
	for index := range result {
		result[index].pendingCommit = jobs[index].pendingCommit
	}
	return result
}

// Caller holds queueMu. Entries themselves never alias live runtime data.
func (e *Engine) putJobLocked(job JobStatus) {
	job.UpdatedAt = e.clock.Now()
	job.pendingCommit = job.Phase != "queued" && job.Phase != "running"
	if e.queuePoisoned && job.Phase == "failed" {
		job.pendingCommit = false
	}
	job = cloneJobStatuses([]JobStatus{job})[0]
	for index := range e.jobs {
		if e.jobs[index].JobID == job.JobID {
			e.jobs[index] = job
			return
		}
	}
	e.jobs = append(e.jobs, job)
	if len(e.jobs) > retainedJobLimit {
		e.jobs = append([]JobStatus{}, e.jobs[len(e.jobs)-retainedJobLimit:]...)
	}
}

func (e *Engine) recordQuotaJob() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.manualQuery.JobID != "" {
		e.putJobLocked(JobStatus{JobID: e.manualQuery.JobID, Kind: "quota_query", Phase: e.manualQuery.Phase, AccountID: e.manualQuery.AccountID, BlockedReason: e.manualQuery.BlockedReason, RetryAt: e.manualQuery.RetryAt, QuotaQuery: &e.manualQuery})
	}
}

func (e *Engine) recordWakeJob() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.manualWake.JobID != "" {
		status := e.manualWake
		if status.Phase == "blocked" && !status.FinishedAt.IsZero() {
			status.Phase = "skipped"
		}
		e.putJobLocked(JobStatus{JobID: status.JobID, Kind: "manual_wake", Phase: status.Phase, AccountID: status.AccountID, BlockedReason: status.BlockedReason, RetryAt: status.RetryAt, ManualWake: &status})
	}
}

func (e *Engine) recordGenerationJob() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	job := e.state.GenerationJob
	if job != nil {
		e.putJobLocked(JobStatus{JobID: job.Request.RequestID, Kind: "prompt_generation", Phase: job.Phase, AccountID: job.Request.AccountID, PromptGeneration: job})
	}
}

// collectJobStatuses only inspects committed account evidence. Progress may be
// published while a network call is running without exposing pending evidence.
func (e *Engine) collectJobStatuses(state core.CoreState) []JobStatus {
	e.queueMu.Lock()
	jobs := cloneJobStatuses(e.jobs)
	e.queueMu.Unlock()
	for index := range jobs {
		job := &jobs[index]
		if job.pendingCommit {
			job.Phase = "running"
			if job.QuotaQuery != nil {
				job.QuotaQuery.Phase = "running"
			}
			if job.ManualWake != nil {
				job.ManualWake.Phase = "running"
			}
			if job.PromptGeneration != nil {
				job.PromptGeneration.Phase = "running"
				job.PromptGeneration.Candidates = []string{}
			}
		}
		if job.ManualWake == nil || job.Phase != "verifying" {
			continue
		}
		account := state.Accounts[job.AccountID]
		if account.Task != nil {
			for _, event := range account.Task.Intents {
				if event.ID == job.ManualWake.EventID && event.Status == core.IntentSatisfied {
					job.Phase, job.ManualWake.Phase = "completed", "completed"
					job.ManualWake.LastError = ""
				}
			}
		}
		// Intent history can outlive the account's current task.
		for _, entry := range state.History {
			for _, eventID := range entry.CoveredEvents {
				if eventID == job.ManualWake.EventID && (entry.Status == "completed" || entry.Status == "verified") {
					job.Phase, job.ManualWake.Phase = "completed", "completed"
					job.ManualWake.FinishedAt = entry.FinishedAt
					job.ManualWake.LastError = ""
					if entry.FinishedAt.After(job.UpdatedAt) {
						job.UpdatedAt = entry.FinishedAt
					}
				} else if eventID == job.ManualWake.EventID && entry.Status == "unknown" && job.Phase != "completed" {
					job.Phase, job.ManualWake.Phase = "unknown", "unknown"
					job.ManualWake.LastError = safeSendErrorCode(entry.ErrorCode)
					if job.ManualWake.LastError == "" {
						job.ManualWake.LastError = "delivery_unconfirmed"
					}
				}
			}
		}
	}
	return jobs
}

// JobStatuses includes operations accepted since the last scheduler step.
func (e *Engine) JobStatuses() []JobStatus {
	if published := e.published.Load(); published != nil {
		return cloneJobStatuses(published.scheduler.Jobs)
	}
	return []JobStatus{}
}

func (e *Engine) blockManualJobs(code string, retryAt time.Time) {
	now := e.clock.Now()
	if e.manualQuery.Phase == "queued" || e.manualQuery.Phase == "running" {
		e.manualQuery.BlockedReason, e.manualQuery.RetryAt = code, retryAt
		if !now.Before(e.manualQuery.StartedAt.Add(manualJobBlockLimit)) {
			e.manualQuery.LastError = code
			e.finishManualQuery("failed")
		} else {
			e.recordQuotaJob()
		}
	}
	if e.manualWake.Phase == "queued" || e.manualWake.Phase == "running" {
		e.manualWake.BlockedReason, e.manualWake.RetryAt = code, retryAt
		if !now.Before(e.manualWake.StartedAt.Add(manualJobBlockLimit)) {
			e.finishManualWake("failed", code)
		} else {
			e.recordWakeJob()
		}
	}
	if job := e.state.GenerationJob; job != nil && (job.Phase == "queued" || job.Phase == "running") {
		if !now.Before(job.StartedAt.Add(manualJobBlockLimit)) {
			e.finishPromptGeneration("failed", code)
		} else {
			e.queueMu.Lock()
			e.putJobLocked(JobStatus{JobID: job.Request.RequestID, Kind: "prompt_generation", Phase: job.Phase, AccountID: job.Request.AccountID, BlockedReason: code, RetryAt: retryAt, PromptGeneration: job})
			e.queueMu.Unlock()
		}
	}
}

func (e *Engine) clearManualJobBlocks() {
	e.manualQuery.BlockedReason, e.manualQuery.RetryAt = "", time.Time{}
	e.manualWake.BlockedReason, e.manualWake.RetryAt = "", time.Time{}
}

func (e *Engine) confirmJobsCommitted() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	for index := range e.jobs {
		e.jobs[index].pendingCommit = false
	}
}

// A failed durable commit poisons the engine. No queued or apparently completed
// operation may advertise success against the older committed account state.
func (e *Engine) failUncommittedJobs() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	e.manualActive, e.manualWakeActive, e.generationActive = false, false, false
	e.queuePoisoned = true
	e.manualRequested, e.manualWakeRequested, e.generationRequested = false, false, false
	for index := range e.jobs {
		job := &e.jobs[index]
		if job.pendingCommit || job.Phase == "queued" || job.Phase == "running" || job.Phase == "dispatched" {
			job.pendingCommit = false
			job.Phase, job.BlockedReason, job.UpdatedAt = "failed", "", e.clock.Now()
			job.RetryAt = time.Time{}
			if job.QuotaQuery != nil {
				job.QuotaQuery.Phase, job.QuotaQuery.LastError = "failed", "durable_commit_failed"
				job.QuotaQuery.BlockedReason, job.QuotaQuery.RetryAt = "", time.Time{}
				if e.manualQuery.JobID == job.JobID {
					e.manualQuery.Phase, e.manualQuery.LastError = "failed", "durable_commit_failed"
					e.manualQuery.BlockedReason, e.manualQuery.RetryAt = "", time.Time{}
				}
			}
			if job.ManualWake != nil {
				job.ManualWake.Phase, job.ManualWake.LastError = "failed", "durable_commit_failed"
				job.ManualWake.BlockedReason, job.ManualWake.RetryAt = "", time.Time{}
				if e.manualWake.JobID == job.JobID {
					e.manualWake.Phase, e.manualWake.LastError = "failed", "durable_commit_failed"
					e.manualWake.BlockedReason, e.manualWake.RetryAt = "", time.Time{}
				}
			}
			if job.PromptGeneration != nil {
				job.PromptGeneration.Phase, job.PromptGeneration.ErrorCode = "failed", "durable_commit_failed"
			}
		}
	}
}
