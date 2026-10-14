package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"codex-window-refresh/internal/core"
)

type ManualWakeStatus struct {
	AccountID  core.AccountID            `json:"account_id,omitempty"`
	Generation core.CredentialGeneration `json:"-"`
	Phase      string                    `json:"phase"`
	LastError  string                    `json:"last_error,omitempty"`
	EventID    core.EventID              `json:"event_id,omitempty"`
	StartedAt  time.Time                 `json:"started_at"`
	FinishedAt time.Time                 `json:"finished_at"`
}

func (e *Engine) QueueManualWake(id core.AccountID) string {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.manualWakeActive {
		if e.manualWakeTarget == id {
			return "already_queued"
		}
		return "busy"
	}
	if e.manualActive || e.generationActive {
		return "busy"
	}
	account, exists := e.Snapshot().Accounts[id]
	if id == "" || !exists {
		return "account_not_found"
	}
	if account.Task != nil && account.Task.OperationID != "" {
		return "delivery_evidence_pending"
	}
	e.manualWakeRequested, e.manualWakeActive = true, true
	e.manualWakeTarget, e.manualWakeGeneration = id, account.Generation
	e.Notify()
	return "queued"
}

func (e *Engine) startManualWake() {
	e.queueMu.Lock()
	requested, target, generation := e.manualWakeRequested, e.manualWakeTarget, e.manualWakeGeneration
	e.manualWakeRequested = false
	e.queueMu.Unlock()
	if requested {
		e.manualWake = ManualWakeStatus{AccountID: target, Generation: generation, Phase: "queued", StartedAt: e.clock.Now()}
		e.nextInventory = time.Time{}
	}
}

func (e *Engine) finishManualWake(phase, code string) {
	e.manualWake.Phase, e.manualWake.LastError = phase, code
	e.manualWake.FinishedAt = e.clock.Now()
	if e.manualWake.EventID == "" && (phase == "blocked" || phase == "failed") {
		status := "failed"
		if phase == "blocked" {
			status = "skipped"
		}
		core.AppendHistory(&e.state, core.HistoryEntry{Kind: "wake", AccountID: e.manualWake.AccountID, Generation: e.manualWake.Generation, Status: status, ErrorCode: code, StartedAt: e.manualWake.StartedAt, FinishedAt: e.manualWake.FinishedAt, Sources: []core.ActionSource{{Trigger: "manual_wake"}}})
	}
	e.queueMu.Lock()
	e.manualWakeActive = false
	e.queueMu.Unlock()
}

func (e *Engine) stepManualWake(ctx context.Context) error {
	id := e.manualWake.AccountID
	account, exists := e.state.Accounts[id]
	if !exists || account.Generation != e.manualWake.Generation {
		e.finishManualWake("blocked", "credential_changed")
		return nil
	}
	if !account.Enabled || e.cfg.ManualWakeDisabled {
		e.finishManualWake("blocked", "account_disabled")
		return nil
	}
	if strings.TrimSpace(e.cfg.Execution.Model) == "" {
		e.finishManualWake("blocked", "model_required")
		return nil
	}
	if !e.caps.StableAccountIdentity || !e.caps.ExactAccountExecution || !e.caps.QuotaAdapterReady {
		e.finishManualWake("blocked", "host_capability_unavailable")
		return nil
	}
	if account.Task != nil && account.Task.OperationID != "" {
		e.finishManualWake("blocked", "delivery_evidence_pending")
		return nil
	}
	now := e.clock.Now()
	if !e.state.LastDispatchAt.IsZero() && now.Before(e.state.LastDispatchAt.Add(e.cfg.Runtime().MinDispatchSpacing)) {
		e.finishManualWake("blocked", "dispatch_spacing")
		return nil
	}
	if now.Before(account.BackoffUntil) || now.Before(e.nextPreflight[id]) || core.ForegroundBusy(e.state, id, account.Generation) {
		code := "backoff"
		if core.ForegroundBusy(e.state, id, account.Generation) {
			code = "foreground_busy"
		}
		e.finishManualWake("blocked", code)
		return nil
	}
	e.manualWake.Phase = "running"
	readFence := e.state.NextReadSequence
	if err := e.observe(ctx, id); err != nil {
		e.finishManualWake("failed", quotaReadErrorCode(err))
		return nil
	}
	if ctx.Err() != nil {
		e.finishManualWake("failed", "request_cancelled")
		return nil
	}
	now = e.clock.Now()
	account = e.state.Accounts[id]
	if account.Generation != e.manualWake.Generation || account.FiveHour.ReadStartedSeq <= readFence || account.PermissionAcceptedSeq <= readFence {
		e.finishManualWake("blocked", "quota_unknown")
		return nil
	}
	event := core.Intent{ID: core.EventID(fmt.Sprintf("%s/%s/manual/%d", id, account.Generation, core.NextSequence(&e.state))), Reason: core.ReasonManual, OriginalDueAt: now, RandomizedDueAt: now, CreatedAt: now, Status: core.IntentPending}
	task := core.MergedTask{AccountID: id, Generation: account.Generation, State: core.TaskPending, Intents: []core.Intent{event}}
	admission := core.DecideAdmission(core.AdmissionInput{Now: now, Enabled: account.Enabled, AccountID: id, Generation: account.Generation, Task: task, FiveHour: account.FiveHour, FiveHourBaselineResetAt: account.FiveHourBaseline.StableResetAt, QuotaPermission: account.Permission, QuotaObservedAt: account.PermissionObservedAt, QuotaMaxAge: e.cfg.Runtime().EvidenceMaxAge, ForegroundBusy: core.ForegroundBusy(e.state, id, account.Generation), BackoffUntil: account.BackoffUntil, ConcurrencySlotAvailable: true})
	if !admission.Allowed {
		e.finishManualWake("blocked", string(admission.Hold))
		return nil
	}
	if account.Task == nil {
		account.Task = &task
	} else {
		account.Task.Intents = append(account.Task.Intents, event)
		account.Task.State = core.TaskPending
	}
	e.state.Accounts[id] = account
	e.manualWake.EventID = event.ID
	if err := e.dispatch(ctx, id, admission); err != nil {
		e.finishManualWake("failed", "durable_commit_failed")
		return err
	}
	account = e.state.Accounts[id]
	if account.Task != nil && account.Task.SendOutcome == core.SendDefinitelyNotSent {
		for index := range account.Task.Intents {
			if account.Task.Intents[index].ID == event.ID {
				account.Task.Intents[index].Status = core.IntentSuperseded
			}
		}
		e.state.Accounts[id] = account
		e.finishManualWake("failed", "send_not_sent")
	} else {
		e.finishManualWake("verifying", "")
	}
	return nil
}
