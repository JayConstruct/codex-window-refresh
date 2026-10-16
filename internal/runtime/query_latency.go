package runtime

import (
	"context"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
)

// ManualQueryTiming measures the visible operation with a monotonic clock.
// Durations do not include browser polling or expose upstream response data.
type ManualQueryTiming struct {
	QueuedMS    int64 `json:"queued_ms"`
	InventoryMS int64 `json:"inventory_ms"`
	QueryMS     int64 `json:"query_ms"`
	CommitMS    int64 `json:"commit_ms"`
	TotalMS     int64 `json:"total_ms"`
}

type selectedAccountReader interface {
	Account(context.Context, core.AccountID) (core.HostAccount, error)
}

func (e *Engine) canQuerySelectedAccount() bool {
	_, supported := e.host.(selectedAccountReader)
	_, known := e.state.Accounts[e.manualQuery.AccountID]
	return supported && e.inventory != nil && known && e.manualQuery.AccountID != "" &&
		(e.manualQuery.Phase == "queued" || e.manualQuery.Phase == "running")
}

func (e *Engine) stepSelectedAccountQuery(ctx context.Context, before [32]byte) error {
	id := e.manualQuery.AccountID
	if !e.caps.QuotaAdapterReady {
		e.stepManualQuery(ctx)
		return e.commitChanged(ctx, before)
	}
	started := time.Now()
	accountCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.QuotaReadTimeout.Value())
	account, err := e.host.(selectedAccountReader).Account(accountCtx, id)
	if err == nil && accountCtx.Err() != nil {
		err = accountCtx.Err()
	}
	cancel()
	e.measureManualInventory(time.Since(started))
	if err == nil && (account.ID != id || account.Generation == "") {
		err = errors.New("account_binding_changed")
	}
	if err != nil {
		// This is the selected credential's failure, rather than a pool-wide
		// dependency outage. Release the slot without querying unrelated accounts.
		e.manualQuery.Total, e.manualQuery.Failed = 1, 1
		e.manualQuery.LastError = quotaReadErrorCode(err)
		diagnostic := quotaReadDiagnostic(err, time.Since(started))
		diagnostic.Stage = "binding"
		e.manualQuery.Diagnostic = &diagnostic
		e.lastError, e.lastQuotaDiagnostic = e.manualQuery.LastError, &diagnostic
		core.AppendHistory(&e.state, core.HistoryEntry{Kind: "query", AccountID: id, Generation: e.state.Accounts[id].Generation,
			StartedAt: e.manualQuery.StartedAt, FinishedAt: e.clock.Now(), Status: "failed", ErrorCode: e.manualQuery.LastError,
			Sources: []core.ActionSource{{Trigger: "manual_query"}}})
		e.finishManualQuery("completed")
		return e.commitChanged(ctx, before)
	}
	e.reconcileSelectedAccount(account)
	e.clearManualJobBlocks()
	e.drain()
	e.stepManualQuery(ctx)
	return e.commitChanged(ctx, before)
}

// refreshSelectedAccount only refreshes the selected host identity and status.
func (e *Engine) refreshSelectedAccount(ctx context.Context, id core.AccountID) error {
	readCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.QuotaReadTimeout.Value())
	defer cancel()
	account, err := e.host.(selectedAccountReader).Account(readCtx, id)
	if err != nil {
		return err
	}
	if err := readCtx.Err(); err != nil {
		return err
	}
	if account.ID != id || account.Generation == "" {
		return errors.New("account_binding_changed")
	}
	e.reconcileSelectedAccount(account)
	return nil
}

func (e *Engine) reconcileSelectedAccount(account core.HostAccount) {
	id := account.ID
	// Preserve other accounts: the single entry is not an authoritative pool.
	found := false
	for index := range e.inventory {
		if e.inventory[index].ID == id {
			e.inventory[index], found = account, true
			break
		}
	}
	if !found {
		e.inventory = append(e.inventory, account)
	}
	core.ReconcileAccount(&e.state, account)
	state := e.state.Accounts[id]
	policy := e.cfg.Account(id)
	state.Enabled = state.Enabled && !policy.Paused
	state.OrdinaryParticipant, state.WeeklyEnabled = policy.OrdinaryParticipant, policy.WeeklyEnabled
	e.state.Accounts[id] = state
}

func (e *Engine) manualQueryWaiting() bool {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	return e.manualRequested
}

func (e *Engine) refreshManualTiming() {
	if e.manualQuery.Timing != nil && !e.manualExecutionStarted.IsZero() {
		e.manualQuery.Timing.TotalMS = e.manualQuery.Timing.QueuedMS + max(0, time.Since(e.manualExecutionStarted).Milliseconds())
	}
}

func (e *Engine) measureManualInventory(elapsed time.Duration) {
	if e.manualQuery.Timing != nil && (e.manualQuery.Phase == "queued" || e.manualQuery.Phase == "running") {
		e.manualQuery.Timing.InventoryMS += max(0, elapsed.Milliseconds())
		e.refreshManualTiming()
	}
}

func (e *Engine) measureManualCommit(elapsed time.Duration) {
	if e.manualQuery.Timing == nil || e.manualQuery.JobID == "" {
		return
	}
	// A finished task remains the last manual task during later unrelated
	// commits. Its measured total must stop advancing after its first commit.
	if e.manualQuery.Phase != "queued" && e.manualQuery.Phase != "running" && e.manualExecutionStarted.IsZero() {
		return
	}
	e.manualQuery.Timing.CommitMS += max(0, elapsed.Milliseconds())
	e.refreshManualTiming()
	e.recordQuotaJob()
	if e.manualQuery.Phase != "queued" && e.manualQuery.Phase != "running" {
		e.manualExecutionStarted = time.Time{}
	}
}
