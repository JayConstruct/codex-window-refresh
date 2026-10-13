package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"codex-window-refresh/internal/core"
)

func (e *Engine) dispatch(ctx context.Context, id core.AccountID, admission core.Admission) error {
	account := e.state.Accounts[id]
	now := e.clock.Now()
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
	if err := e.commit(ctx); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.RequestTimeout.Value())
	result, sendErr := e.host.SendExact(sendCtx, core.ExactRequest{AccountID: id, Generation: account.Generation, OperationID: operation, ReadStartedSeq: sequence, Model: e.cfg.Execution.Model, Prompt: e.cfg.Execution.Prompt})
	if sendCtx.Err() != nil && result.Disposition != core.SendDefinitelyNotSent {
		result.Disposition = core.SendUnknown
	}
	cancel()
	if result.Disposition == "" {
		result.Disposition = core.SendUnknown
	}
	if sendErr != nil {
		e.lastError = "send_failed"
	}
	finished := e.clock.Now()
	if result.Disposition == core.SendDefinitelyNotSent && result.RetryAfter.IsZero() {
		result.RetryAfter = finished.Add(time.Minute)
	}
	core.ApplySendResult(&e.state, id, operation, result, finished)
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
