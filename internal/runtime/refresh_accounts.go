package runtime

import (
	"context"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
)

// RefreshAccounts synchronizes CPA's local inventory without running a scheduler
// step, reading upstream quota, or notifying the execution loop.
func (e *Engine) RefreshAccounts(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.publish(false)
	if e.poisoned {
		return errors.New("durable_commit_failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.QuotaReadTimeout.Value())
	defer cancel()
	accounts, err := e.host.Accounts(readCtx)
	if err == nil {
		err = readCtx.Err()
	}
	if err != nil {
		e.lastError = "account_read_failed"
		return errors.New("account_read_failed")
	}
	now := e.clock.Now()
	e.inventory = append([]core.HostAccount{}, accounts...)
	e.inventoryFailed = false
	e.nextInventory = now.Add(30 * time.Second)
	if e.lastError == "account_read_failed" {
		e.lastError = ""
	}
	e.reconcileInventory(now)
	// Publish account state and inventory diagnostics only after durable success,
	// even when only the host's diagnostic information has changed.
	return e.commit(ctx)
}

func (e *Engine) reconcileInventory(now time.Time) {
	core.ReconcileAccounts(&e.state, e.inventory, now)
	if catalog, ok := e.host.(interface {
		GenerationAccounts() map[core.AccountID]core.GenerationAccount
	}); ok {
		e.state.GenerationAccounts = catalog.GenerationAccounts()
	}
	for id := range e.nextRead {
		if _, ok := e.state.Accounts[id]; !ok {
			delete(e.nextRead, id)
			delete(e.nextPreflight, id)
			delete(e.readFailures, id)
			delete(e.activityEpoch, id)
		}
	}
	for id, account := range e.state.Accounts {
		policy := e.cfg.Account(id)
		account.Enabled = account.Enabled && !policy.Paused
		account.OrdinaryParticipant = policy.OrdinaryParticipant
		account.WeeklyEnabled = policy.WeeklyEnabled
		e.state.Accounts[id] = account
	}
}
