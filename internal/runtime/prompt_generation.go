package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type promptGenerator interface {
	GeneratePrompts(context.Context, core.ExactRequest, int) (core.SendResult, []string, error)
}

func generationConfigPath(target string) string {
	if target == "default" {
		return "execution.prompt_pool"
	}
	return "reset_rules." + target + ".prompt_pool"
}

func (e *Engine) QueuePromptGeneration(request core.PromptGenerationRequest) string {
	for _, character := range request.RequestID {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '-' && character != '_' {
			return "invalid_generation_parameters"
		}
	}
	if !utf8.ValidString(request.Model) || strings.ContainsAny(request.Model, "\x00\r\n") {
		return "invalid_generation_parameters"
	}
	if len(request.RequestID) < 8 || len(request.RequestID) > 128 || strings.TrimSpace(request.Model) == "" || len(request.Model) > 256 || request.Count < 5 || request.Count > 50 || (request.Target != "default" && request.Target != "five_hour" && request.Target != "weekly") {
		return "invalid_generation_parameters"
	}
	if _, ok := e.host.(promptGenerator); !ok {
		return "prompt_generation_unavailable"
	}
	e.queueMu.Lock()
	publishQueued := false
	defer func() {
		e.queueMu.Unlock()
		if publishQueued {
			e.publishQueuedJobs()
		}
	}()
	state := e.Snapshot()
	if e.queuePoisoned || e.lastPublishedCommitFailed() {
		return "durable_commit_failed"
	}
	for _, previous := range e.jobs {
		if previous.JobID == request.RequestID {
			if previous.PromptGeneration != nil && previous.PromptGeneration.Request == request {
				publishQueued = true
				return "already_queued"
			}
			return "request_id_conflict"
		}
	}
	if e.generationActive {
		if e.generationRequest == request {
			publishQueued = true
			return "already_queued"
		}
		return "busy"
	}
	if state.GenerationJob != nil && state.GenerationJob.Request.RequestID == request.RequestID {
		if state.GenerationJob.Request == request {
			return "already_queued"
		}
		return "request_id_conflict"
	}
	for _, entry := range state.History {
		if entry.OperationID == core.OperationID("generation/"+request.RequestID) {
			return "request_id_conflict"
		}
	}
	if e.manualActive || e.manualWakeActive {
		return "busy"
	}
	account, exists := state.Accounts[request.AccountID]
	if !exists {
		return "account_not_found"
	}
	if !account.Enabled {
		return "account_disabled"
	}
	if account.Task != nil && account.Task.OperationID != "" {
		return "delivery_evidence_pending"
	}
	e.generationRequest, e.generationAccountGeneration = request, account.Generation
	e.generationRequested, e.generationActive = true, true
	queued := core.PromptGenerationJob{Request: request, Generation: account.Generation, Phase: "queued", StartedAt: e.clock.Now(), Candidates: []string{}}
	e.putJobLocked(JobStatus{JobID: request.RequestID, Kind: "prompt_generation", Phase: "queued", AccountID: request.AccountID, PromptGeneration: &queued})
	publishQueued = true
	e.Notify()
	return "queued"
}

func (e *Engine) startPromptGeneration() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if !e.generationRequested {
		return
	}
	e.generationRequested = false
	e.state.GenerationJob = &core.PromptGenerationJob{Request: e.generationRequest, Generation: e.generationAccountGeneration, Phase: "queued", StartedAt: e.clock.Now(), Candidates: []string{}}
	e.nextInventory = time.Time{}
}

func (e *Engine) finishPromptGeneration(phase, code string) {
	job := e.state.GenerationJob
	job.Phase, job.ErrorCode, job.FinishedAt = phase, code, e.clock.Now()
	for index := range e.state.History {
		entry := &e.state.History[index]
		if entry.ID == job.HistoryID {
			entry.Status, entry.ErrorCode, entry.FinishedAt = phase, code, job.FinishedAt
		}
	}
	if job.HistoryID == 0 {
		job.HistoryID = core.AppendHistory(&e.state, core.HistoryEntry{Kind: "generation", AccountID: job.Request.AccountID, Generation: job.Generation, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, Status: phase, ErrorCode: code, Model: job.Request.Model, Sources: []core.ActionSource{{Trigger: "prompt_generation", ConfigPath: generationConfigPath(job.Request.Target)}}})
	}
	e.recordGenerationJob()
	e.queueMu.Lock()
	e.generationActive = false
	e.queueMu.Unlock()
}

func (e *Engine) stepPromptGeneration(ctx context.Context) error {
	job := e.state.GenerationJob
	id, now := job.Request.AccountID, e.clock.Now()
	account, exists := e.state.Accounts[id]
	blocked := ""
	switch {
	case !exists || account.Generation != job.Generation:
		blocked = "credential_changed"
	case !account.Enabled || e.cfg.ManualWakeDisabled:
		blocked = "account_disabled"
	case !e.caps.StableAccountIdentity || !e.caps.ExactAccountExecution || !e.caps.QuotaAdapterReady:
		blocked = "host_capability_unavailable"
	case account.Task != nil && account.Task.OperationID != "":
		blocked = "delivery_evidence_pending"
	case core.ForegroundBusy(e.state, id, account.Generation):
		blocked = "foreground_busy"
	case now.Before(account.BackoffUntil) || now.Before(e.nextPreflight[id]):
		blocked = "backoff"
	case !e.state.LastDispatchAt.IsZero() && now.Before(e.state.LastDispatchAt.Add(e.cfg.Runtime().MinDispatchSpacing)):
		blocked = "dispatch_spacing"
	}
	if blocked != "" {
		e.finishPromptGeneration("skipped", blocked)
		return nil
	}
	job.Phase = "running"
	e.recordGenerationJob()
	e.publish(false)
	fence := e.state.NextReadSequence
	if err := e.observe(ctx, id, core.ActionSource{Trigger: "generation_preflight", ConfigPath: "execution.quota_read_timeout"}); err != nil {
		e.finishPromptGeneration("failed", quotaReadErrorCode(err))
		return nil
	}
	now, account = e.clock.Now(), e.state.Accounts[id]
	if ctx.Err() != nil || account.Generation != job.Generation || account.PermissionAcceptedSeq <= fence || account.Permission != core.PermissionAllowed || core.ForegroundBusy(e.state, id, account.Generation) || !account.Enabled {
		e.finishPromptGeneration("skipped", "quota_or_binding_unavailable")
		return nil
	}
	operation := core.OperationID("generation/" + job.Request.RequestID)
	event := core.Intent{ID: core.EventID(fmt.Sprintf("%s/%s/%s", id, account.Generation, operation)), Reason: core.ReasonGeneration, OriginalDueAt: now, RandomizedDueAt: now, CreatedAt: now, Status: core.IntentPending}
	if account.Task == nil {
		account.Task = &core.MergedTask{AccountID: id, Generation: account.Generation}
	}
	account.Task.Intents = append(account.Task.Intents, event)
	account.Task.State = core.TaskPending
	account.Task.SelectedModel, account.Task.SelectedPrompt = job.Request.Model, core.GenerationPrompt(job.Request.Count)
	account.Task.GenerationBaselineResetAt = account.FiveHour.ResetAt
	e.state.Accounts[id] = account
	e.state.Reservations = append(e.state.Reservations, core.Reservation{ID: core.ReservationID(operation), AccountID: id, Generation: account.Generation, CapacityHoldUntil: now.Add(e.cfg.Runtime().VerificationCapacityHold)})
	if err := core.MarkDispatch(&e.state, id, operation, core.ReservationID(operation), []core.EventID{event.ID}, now); err != nil {
		return err
	}
	sequence := core.NextSequence(&e.state)
	account.Task.DispatchReadSequence = sequence
	job.OperationID, job.Phase = operation, "dispatched"
	job.HistoryID = core.AppendHistory(&e.state, core.HistoryEntry{Kind: "generation", AccountID: id, Generation: account.Generation, OperationID: operation, Model: job.Request.Model, Prompt: account.Task.SelectedPrompt, StartedAt: job.StartedAt, Status: "dispatched", CoveredEvents: []core.EventID{event.ID}, Sources: []core.ActionSource{{Trigger: "prompt_generation", ConfigPath: generationConfigPath(job.Request.Target), EventID: event.ID}}})
	e.recordGenerationJob()
	if err := e.commit(ctx); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.RequestTimeout.Value())
	result, candidates, err := e.host.(promptGenerator).GeneratePrompts(sendCtx, core.ExactRequest{AccountID: id, Generation: account.Generation, OperationID: operation, ReadStartedSeq: sequence, Model: job.Request.Model, Prompt: account.Task.SelectedPrompt}, job.Request.Count)
	if sendCtx.Err() != nil && result.Disposition != core.SendDefinitelyNotSent {
		result.Disposition = core.SendUnknown
	}
	cancel()
	if result.Disposition == "" {
		result.Disposition = core.SendUnknown
	}
	finished := e.clock.Now()
	core.ApplySendResult(&e.state, id, operation, result, finished)
	account = e.state.Accounts[id]
	if account.Task != nil {
		if result.Disposition != core.SendUnknown {
			for index := range account.Task.Intents {
				if account.Task.Intents[index].ID == event.ID {
					account.Task.Intents[index].Status = core.IntentSatisfied
				}
			}
			e.state.Accounts[id] = account
			core.ReconcileVerification(&e.state, id, finished)
		} else {
			account.Task.VerificationStartedAt, account.Task.NextVerificationAt = finished, finished.Add(e.cfg.Runtime().VerifyDelays[0])
			e.state.Accounts[id] = account
		}
	}
	switch {
	case result.Disposition == core.SendDefinitelyNotSent:
		e.finishPromptGeneration("failed", "send_not_sent")
	case result.Disposition == core.SendUnknown:
		e.finishPromptGeneration("unknown", "delivery_unconfirmed")
	case err != nil || len(candidates) != job.Request.Count || config.ValidatePromptPool(candidates, true) != nil:
		e.finishPromptGeneration("failed", "prompt_candidates_invalid")
	default:
		job.Candidates = append([]string{}, candidates...)
		e.finishPromptGeneration("completed", "")
	}
	e.drain()
	return e.commit(ctx)
}
