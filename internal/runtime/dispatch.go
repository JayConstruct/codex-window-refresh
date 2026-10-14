package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
)

func (e *Engine) dispatch(ctx context.Context, id core.AccountID, admission core.Admission) error {
	account := e.state.Accounts[id]
	now := e.clock.Now()
	reason := core.ReasonFiveHour
	if admission.WeeklyPriority {
		reason = core.ReasonWeekly
	}
	for _, intent := range account.Task.Intents {
		for _, covered := range admission.CoveredEventIDs {
			if intent.ID == covered && intent.Reason == core.ReasonManual {
				reason = core.ReasonManual
			}
		}
	}
	model, prompt := e.cfg.WakeContent(reason)
	if e.cfg.Execution.PromptMode == "random" {
		pool := e.cfg.WakePromptPool(reason)
		if len(pool) == 0 {
			return errors.New("prompt_pool_empty")
		}
		index, err := e.random.Duration(0, time.Duration(len(pool)-1))
		if err != nil {
			return err
		}
		if index < 0 || int64(index) >= int64(len(pool)) {
			return errors.New("prompt_random_out_of_range")
		}
		prompt = pool[int(index)]
	}
	account.Task.SelectedModel, account.Task.SelectedPrompt = model, prompt
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return err
	}
	operation := core.OperationID(hex.EncodeToString(randomBytes[:]))
	reservation := core.ReservationID(operation)
	e.state.Reservations = append(e.state.Reservations, core.Reservation{ID: reservation, AccountID: id, Generation: account.Generation, Weekly: admission.WeeklyPriority, CapacityHoldUntil: now.Add(e.cfg.Runtime().VerificationCapacityHold)})
	if err := core.MarkDispatch(&e.state, id, operation, reservation, admission.CoveredEventIDs, now); err != nil {
		return err
	}
	sequence := core.NextSequence(&e.state)
	account.Task.DispatchReadSequence = sequence
	historyID := core.AppendHistory(&e.state, core.HistoryEntry{Kind: "wake", AccountID: id, Generation: account.Generation, OperationID: operation, StartedAt: now, Status: "dispatched", Model: model, Prompt: boundedHistoryPrompt(prompt), CoveredEvents: admission.CoveredEventIDs, Sources: wakeSources(account, admission.CoveredEventIDs)})
	if err := e.commit(ctx); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.RequestTimeout.Value())
	result, sendErr := e.host.SendExact(sendCtx, core.ExactRequest{AccountID: id, Generation: account.Generation, OperationID: operation, ReadStartedSeq: sequence, Model: model, Prompt: prompt})
	if sendCtx.Err() != nil && result.Disposition != core.SendDefinitelyNotSent {
		result.Disposition = core.SendUnknown
	}
	cancel()
	if result.Disposition == "" {
		result.Disposition = core.SendUnknown
	}
	sendCode := safeSendErrorCode(result.ErrorCode)
	if sendCode == "" && sendErr != nil {
		sendCode = "send_failed"
		if result.Disposition == core.SendDefinitelyNotSent {
			sendCode = "send_not_sent"
		}
	}
	if sendCode != "" {
		e.lastError = sendCode
	}
	finished := e.clock.Now()
	if result.Disposition == core.SendDefinitelyNotSent && result.RetryAfter.IsZero() {
		result.RetryAfter = finished.Add(time.Minute)
	}
	core.ApplySendResult(&e.state, id, operation, result, finished)
	for index := range e.state.History {
		entry := &e.state.History[index]
		if entry.ID != historyID {
			continue
		}
		entry.Status = "unknown"
		entry.ErrorCode = sendCode
		if result.Disposition == core.SendCompleted {
			entry.Status = "verifying"
		}
		if result.Disposition == core.SendDefinitelyNotSent {
			entry.Status, entry.FinishedAt = "failed", finished
			if entry.ErrorCode == "" {
				entry.ErrorCode = "send_not_sent"
			}
		}
	}
	for index := range e.state.Reservations {
		if e.state.Reservations[index].OperationID == operation {
			e.state.Reservations[index].CapacityHoldUntil = finished.Add(e.cfg.Runtime().VerificationCapacityHold)
		}
	}
	account = e.state.Accounts[id]
	if account.Task != nil && account.Task.OperationID != "" {
		account.Task.VerificationStartedAt = finished
		account.Task.VerificationAttempt = 0
		account.Task.NextVerificationAt = finished.Add(e.cfg.Runtime().VerifyDelays[0])
		e.state.Accounts[id] = account
	}
	e.drain()
	e.reconcileDailyCapacity(finished)
	return e.commit(ctx)
}

// Host errors may contain credentials or upstream response bodies. Persist only
// stable classifications understood by this runtime; never reflect error text.
func safeSendErrorCode(code string) string {
	switch code {
	case "", "send_failed", "send_not_sent", "invalid_exact_request", "account_identity_unavailable", "account_binding_changed", "host_callback_failed",
		"model_callback_failed", "model_stream_missing", "model_stream_read_failed", "model_stream_frame_too_large", "model_stream_canceled", "model_stream_missing_completion", "model_stream_failed":
		return code
	default:
		return "send_failed"
	}
}
