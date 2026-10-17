package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type AccountPage struct {
	InstanceID       string    `json:"instance_id,omitempty"`
	Revision         uint64    `json:"view_revision"`
	AccountsRevision uint64    `json:"accounts_revision"`
	Accounts         []Account `json:"accounts"`
	Next             string    `json:"next_after"`
	Total            int       `json:"total"`
	FilterTotal      int       `json:"filter_total"`
	Offset           int       `json:"offset"`
}

func (s *Store) AccountPage(ctx context.Context, after string, limit int, expected *uint64) (AccountPage, error) {
	return s.AccountPageFiltered(ctx, after, limit, expected, "", "all")
}

func (s *Store) AccountPageFiltered(ctx context.Context, after string, limit int, expected *uint64, search, status string) (AccountPage, error) {
	page := AccountPage{Accounts: []Account{}}
	if limit < 1 || limit > 500 || len(search) > 256 {
		return page, errors.New("invalid_account_filter")
	}
	filter := "status!='removed' AND instr(lower(id),lower(?))>0"
	switch status {
	case "", "all":
	case "available", "disabled":
		filter += " AND status='" + status + "'"
	case "unavailable":
		filter += " AND status NOT IN ('available','disabled')"
	case "attention":
		filter += " AND (status NOT IN ('available','disabled') OR CASE WHEN json_valid(quota) THEN json_extract(quota,'$.permission')='blocked' ELSE 0 END)"
	default:
		return page, errors.New("invalid_account_filter")
	}
	if strings.ContainsAny(search, "\x00\r\n") {
		return page, errors.New("invalid_account_filter")
	}
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
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE status!='removed'").Scan(&page.Total); err != nil {
		return page, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE "+filter, search).Scan(&page.FilterTotal); err != nil {
		return page, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE "+filter+" AND id<=?", search, after).Scan(&page.Offset); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,auth_index,status,COALESCE(quota,''),COALESCE(observed_at,'') FROM accounts WHERE "+filter+" AND id>? ORDER BY id LIMIT ?", search, after, limit+1)
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
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return page, err
	}
	if len(page.Accounts) > limit {
		page.Accounts = page.Accounts[:limit]
		page.Next = page.Accounts[len(page.Accounts)-1].ID
	}
	for index := range page.Accounts {
		page.Accounts[index].PendingQuery, err = readPendingQuery(ctx, tx, page.Accounts[index].ID, settings.Config)
		if err != nil {
			return page, err
		}
	}
	return page, tx.Commit()
}
