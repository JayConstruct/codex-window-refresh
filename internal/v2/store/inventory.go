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

func (s *Store) SyncInventory(ctx context.Context, accounts []Account) error {
	if len(accounts) > 10000 {
		return errors.New("inventory_too_large")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS current_inventory(id TEXT PRIMARY KEY); DELETE FROM current_inventory"); err != nil {
			return err
		}
		for _, a := range accounts {
			if !ValidID(a.ID) || len(a.AuthIndex) > 256 || len(a.Binding) > 256 {
				return errors.New("invalid_account_inventory")
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO current_inventory VALUES(?)", a.ID); err != nil {
				return err
			}
			var index, binding string
			err := tx.QueryRowContext(ctx, "SELECT a.auth_index,COALESCE(b.binding,'') FROM accounts a LEFT JOIN account_bindings b ON b.account_id=a.id WHERE a.id=?", a.ID).Scan(&index, &binding)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			changed := err == nil && (index != a.AuthIndex || binding != a.Binding)
			if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,auth_index,status,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET auth_index=excluded.auth_index,status=excluded.status,quota=CASE WHEN ? THEN NULL ELSE accounts.quota END,observed_at=CASE WHEN ? THEN NULL ELSE accounts.observed_at END,updated_at=excluded.updated_at`, a.ID, a.AuthIndex, a.Status, now(), changed, changed); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO account_bindings VALUES(?,?,?) ON CONFLICT(account_id) DO UPDATE SET binding=excluded.binding,confirmed_at=excluded.confirmed_at", a.ID, a.Binding, now()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET status='removed',quota=NULL,observed_at=NULL WHERE id NOT IN(SELECT id FROM current_inventory) AND status!='removed'"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_bindings WHERE account_id NOT IN(SELECT id FROM current_inventory)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE inventory_state SET status='ready',error_code='',last_success_at=? WHERE singleton=1", now())
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
