package runtime

import (
	"context"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

// Antigravity generation is an explicit content task. Its quota and completion
// evidence must never enter the Codex five-hour/weekly wake state machine.
func (e *Engine) stepAntigravityGeneration(ctx context.Context) error {
	job, now := e.state.GenerationJob, e.clock.Now()
	account, exists := e.state.GenerationAccounts[job.Request.AccountID]
	e.drain()
	blocked := ""
	switch {
	case !exists || account.Provider != "antigravity" || account.Generation != job.Generation:
		blocked = "credential_changed"
	case !account.Enabled || e.cfg.ManualWakeDisabled:
		blocked = "account_disabled"
	case !e.caps.StableAccountIdentity || !e.caps.ExactAccountExecution:
		blocked = "host_capability_unavailable"
	case core.ForegroundBusy(e.state, account.ID, account.Generation):
		blocked = "foreground_busy"
	case !e.state.LastDispatchAt.IsZero() && now.Before(e.state.LastDispatchAt.Add(e.cfg.Runtime().MinDispatchSpacing)):
		blocked = "dispatch_spacing"
	}
	if blocked != "" {
		e.finishPromptGeneration("skipped", blocked)
		return nil
	}
	job.OperationID, job.Phase = core.OperationID("generation/"+job.Request.RequestID), "dispatched"
	job.HistoryID = core.AppendHistory(&e.state, core.HistoryEntry{Kind: "generation", Provider: "antigravity", AccountID: account.ID, Generation: account.Generation, OperationID: job.OperationID, Model: job.Request.Model, Prompt: core.GenerationPrompt(job.Request.Count), StartedAt: job.StartedAt, Status: "dispatched", Sources: []core.ActionSource{{Trigger: "prompt_generation", ConfigPath: generationConfigPath(job.Request.Target)}}})
	e.state.LastDispatchAt = now
	e.recordGenerationJob()
	// A restart after this boundary records unknown delivery and never retries.
	if err := e.commit(ctx); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.RequestTimeout.Value())
	result, candidates, err := e.host.(promptGenerator).GeneratePrompts(sendCtx, core.ExactRequest{Provider: "antigravity", AccountID: account.ID, Generation: account.Generation, OperationID: job.OperationID, Model: job.Request.Model, Prompt: core.GenerationPrompt(job.Request.Count)}, job.Request.Count)
	if sendCtx.Err() != nil && result.Disposition != core.SendDefinitelyNotSent {
		result.Disposition = core.SendUnknown
	}
	cancel()
	switch {
	case result.Disposition == core.SendDefinitelyNotSent:
		e.finishPromptGeneration("failed", "send_not_sent")
	case result.Disposition != core.SendCompleted:
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
