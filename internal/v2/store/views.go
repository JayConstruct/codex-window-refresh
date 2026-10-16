package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type AccountPage struct {
	InstanceID       string    `json:"instance_id,omitempty"`
	Revision         uint64    `json:"view_revision"`
	AccountsRevision uint64    `json:"accounts_revision"`
	Accounts         []Account `json:"accounts"`
	Next             string    `json:"next_after"`
}

func (s *Store) AccountPage(ctx context.Context, after string, limit int, expected *uint64) (AccountPage, error) {
	page := AccountPage{Accounts: []Account{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision,accounts_revision FROM view_state WHERE singleton=1").Scan(&page.Revision, &page.AccountsRevision); err != nil {
		return page, err
	}
	if expected != nil && *expected != page.AccountsRevision {
		return page, ErrViewChanged
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,auth_index,status,COALESCE(quota,''),COALESCE(observed_at,'') FROM accounts WHERE id>? ORDER BY id LIMIT ?", after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Account
		var quota string
		if err = rows.Scan(&a.ID, &a.AuthIndex, &a.Status, &quota, &a.ObservedAt); err != nil {
			return page, err
		}
		if quota != "" {
			a.Quota = json.RawMessage(quota)
		}
		page.Accounts = append(page.Accounts, a)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	rows.Close()
	if len(page.Accounts) > limit {
		page.Accounts = page.Accounts[:limit]
		page.Next = page.Accounts[len(page.Accounts)-1].ID
	}
	return page, tx.Commit()
}
