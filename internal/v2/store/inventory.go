package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/quota"
)

const InventoryRefreshInterval = 30 * time.Second

// Full inventory verification advances one shared heartbeat. Individual account
// rows only change when their identity or status changes; a query observation
// must still be newer than both its account row and that shared verification.
const accountStatusObservedAtSQL = `MAX(updated_at,COALESCE((SELECT last_success_at FROM inventory_state WHERE singleton=1),''))`

// confirmed_at is a diagnostic checkpoint, not the binding freshness fence.
// inventory_state.last_success_at is refreshed after every full verification.
const inventoryBindingCheckpointInterval = 5 * time.Minute

func applyQueryStatus(ctx context.Context, tx *sql.Tx, run Run, observed *Account) error {
	if observed == nil || observed.ID != run.Account.ID || observed.AuthIndex != run.Account.AuthIndex || observed.Binding == "" || run.Account.Binding != "" && run.Account.Binding != observed.Binding {
		return nil
	}
	switch observed.Status {
	case "available", "disabled", "unavailable":
	default:
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, observed.StatusObservedAt)
	if err != nil || at.IsZero() || at.After(time.Now().UTC()) {
		return nil
	}
	stamp := at.UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET status=?,updated_at=? WHERE id=? AND auth_index=? AND status NOT IN ('removed','binding_unavailable') AND `+accountStatusObservedAtSQL+`<=? AND (COALESCE((SELECT binding FROM account_bindings WHERE account_id=accounts.id),'')=? OR (?='' AND COALESCE((SELECT binding FROM account_bindings WHERE account_id=accounts.id),'')=''))`, observed.Status, stamp, observed.ID, observed.AuthIndex, stamp, observed.Binding, run.Account.Binding)
	return err
}

func (s *Store) SyncInventory(ctx context.Context, accounts []Account) error {
	if len(accounts) > 10000 {
		return errors.New("inventory_too_large")
	}
	for _, a := range accounts {
		if !ValidID(a.ID) || len(a.AuthIndex) > 256 || len(a.Binding) > 256 {
			return errors.New("invalid_account_inventory")
		}
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS current_inventory(
 id TEXT PRIMARY KEY, auth_index TEXT NOT NULL, status TEXT NOT NULL, binding TEXT NOT NULL
); DELETE FROM current_inventory`); err != nil {
			return err
		}
		insert, err := tx.PrepareContext(ctx, "INSERT INTO current_inventory VALUES(?,?,?,?)")
		if err != nil {
			return err
		}
		defer insert.Close()
		for _, a := range accounts {
			if _, err = insert.ExecContext(ctx, a.ID, a.AuthIndex, a.Status, a.Binding); err != nil {
				return err
			}
		}
		at := time.Now().UTC()
		stamp := queryStamp(at)
		// Merge the staged snapshot in sets instead of preparing and executing
		// separate lookups and writes for every account. Binding comparison occurs
		// before replacing account_bindings so changed identities clear old quota.
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET
 auth_index=current.auth_index,status=current.status,
 quota=CASE WHEN accounts.auth_index IS NOT current.auth_index OR COALESCE(previous.binding,'') IS NOT current.binding THEN NULL ELSE accounts.quota END,
 observed_at=CASE WHEN accounts.auth_index IS NOT current.auth_index OR COALESCE(previous.binding,'') IS NOT current.binding THEN NULL ELSE accounts.observed_at END,
 updated_at=?
 FROM current_inventory current LEFT JOIN account_bindings previous ON previous.account_id=current.id
 WHERE accounts.id=current.id AND (accounts.auth_index IS NOT current.auth_index OR accounts.status IS NOT current.status OR COALESCE(previous.binding,'') IS NOT current.binding)`, stamp); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,auth_index,status,updated_at)
 SELECT id,auth_index,status,? FROM current_inventory current
 WHERE NOT EXISTS(SELECT 1 FROM accounts WHERE accounts.id=current.id)`, stamp); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_bindings(account_id,binding,confirmed_at)
 SELECT id,binding,? FROM current_inventory WHERE true
 ON CONFLICT(account_id) DO UPDATE SET binding=excluded.binding,confirmed_at=excluded.confirmed_at
 WHERE account_bindings.binding IS NOT excluded.binding OR account_bindings.confirmed_at<=? OR account_bindings.confirmed_at>excluded.confirmed_at`, stamp, queryStamp(at.Add(-inventoryBindingCheckpointInterval))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET status='removed',quota=NULL,observed_at=NULL WHERE id NOT IN(SELECT id FROM current_inventory) AND status!='removed'"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_bindings WHERE account_id NOT IN(SELECT id FROM current_inventory)"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE inventory_state SET status='ready',error_code='',last_success_at=? WHERE singleton=1", stamp)
		return err
	})
}

type InventoryStatus struct {
	Status                 string `json:"status"`
	ErrorCode              string `json:"error_code,omitempty"`
	LastSuccessAt          string `json:"last_success_at,omitempty"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds"`
}

func (s *Store) InventoryFailed(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE inventory_state SET status='unavailable',error_code='inventory_sync_failed' WHERE singleton=1 AND status!='unavailable'")
		return err
	})
}

func applyQuota(ctx context.Context, tx *sql.Tx, e events.Event) error {
	var old string
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(a.quota,'') FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=? AND b.binding=? AND a.status NOT IN ('removed','binding_unavailable')", e.AccountID, e.AuthIndex, e.Binding).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot, err := quota.Normalize(e.Quota, e.Headers, e.Binding, e.Source, e.At, e.At)
	// Optional malformed provider evidence must not block later lifecycle events.
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	merged, changed, err := quota.Merge([]byte(old), raw)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	var stamp quota.Snapshot
	if err = json.Unmarshal(merged, &stamp); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET quota=?,observed_at=? WHERE id=?", string(merged), stamp.ObservedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), e.AccountID)
	return err
}
