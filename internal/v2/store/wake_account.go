package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type wakeAccountError string

func (err wakeAccountError) Error() string { return string(err) }

func (err wakeAccountError) Unwrap() error { return ErrWakeAccount }

func wakeAccountCode(code string) bool {
	switch code {
	case "wake_account_not_found", "wake_account_disabled", "wake_host_unavailable", "wake_inventory_stale", "wake_account_ambiguous", "wake_binding_missing", "wake_binding_changed", "wake_account_unavailable":
		return true
	}
	return false
}

func WakeAccountErrorCode(err error) string {
	var classified wakeAccountError
	if errors.As(err, &classified) && wakeAccountCode(classified.Error()) {
		return classified.Error()
	}
	if errors.Is(err, ErrWakeAccount) {
		return "wake_account_unavailable"
	}
	if err != nil && wakeAccountCode(err.Error()) {
		return err.Error()
	}
	return ""
}

func wakeAccount(ctx context.Context, tx *sql.Tx, id string, at time.Time) (Account, error) {
	ready, err := wakeInventoryReady(ctx, tx, at)
	if err != nil {
		return Account{}, err
	}
	if !ready {
		return Account{}, wakeAccountError("wake_inventory_stale")
	}
	var account Account
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.auth_index,a.status,COALESCE(a.quota,''),COALESCE(b.binding,'') FROM accounts a LEFT JOIN account_bindings b ON b.account_id=a.id WHERE a.id=?`, id).Scan(&account.ID, &account.AuthIndex, &account.Status, &raw, &account.Binding)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, wakeAccountError("wake_account_not_found")
	}
	if err != nil {
		return Account{}, err
	}
	account.Provider, account.Quota = "codex", json.RawMessage(raw)
	switch account.Status {
	case "removed":
		return account, wakeAccountError("wake_account_not_found")
	case "disabled":
		return account, wakeAccountError("wake_account_disabled")
	case "unavailable":
		return account, wakeAccountError("wake_host_unavailable")
	}
	if strings.TrimSpace(account.AuthIndex) != "" {
		var ambiguous bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE id!=? AND auth_index=? AND status!='removed')", account.ID, account.AuthIndex).Scan(&ambiguous); err != nil {
			return account, err
		}
		if ambiguous {
			return account, wakeAccountError("wake_account_ambiguous")
		}
	}
	if account.Status != "available" {
		return account, ErrWakeAccount
	}
	if strings.TrimSpace(account.AuthIndex) == "" || strings.TrimSpace(account.Binding) == "" {
		return account, wakeAccountError("wake_binding_missing")
	}
	return account, nil
}

func wakeBoundAccount(ctx context.Context, tx *sql.Tx, job WakeJob, at time.Time) (Account, error) {
	account, err := wakeAccount(ctx, tx, job.AccountID, at)
	if err != nil {
		return account, err
	}
	if job.Account.AuthIndex == "" || job.Account.Binding == "" {
		return account, wakeAccountError("wake_binding_missing")
	}
	if account.AuthIndex != job.Account.AuthIndex || account.Binding != job.Account.Binding {
		return account, wakeAccountError("wake_binding_changed")
	}
	return account, nil
}

func wakePreflightFailure(ctx context.Context, tx *sql.Tx, job WakeJob, err error, at time.Time) error {
	code := WakeAccountErrorCode(err)
	if code == "" {
		return err
	}
	if code == "wake_inventory_stale" {
		phase := job.Phase
		if phase == "prepared" {
			phase = "queued"
		}
		return updateWake(ctx, tx, job, phase, "", code, at)
	}
	return updateWake(ctx, tx, job, "failed", code, "", at)
}
