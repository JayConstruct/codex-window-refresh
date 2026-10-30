package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/quota"
)

func (s *Store) AccountPageSorted(ctx context.Context, after string, limit int, expected *uint64, search, status, order string) (AccountPage, error) {
	if order == "" || order == "default" || order == "name" {
		return s.AccountPageFiltered(ctx, after, limit, expected, search, status)
	}
	if order != "five_hour" && order != "weekly" && order != "attention" {
		return AccountPage{}, errors.New("invalid_account_filter")
	}
	return s.accountEvidencePage(ctx, after, limit, expected, search, status, order)
}

// Evidence filters share quotaState with the capacity metric. Filtering runs
// within the same read transaction as the revision and pagination counters.
func (s *Store) accountEvidencePage(ctx context.Context, after string, limit int, expected *uint64, search, status string, orders ...string) (AccountPage, error) {
	page := AccountPage{Accounts: []Account{}}
	if limit < 1 || limit > 500 || len(search) > 256 || strings.ContainsAny(search, "\x00\r\n") {
		return page, errors.New("invalid_account_filter")
	}
	switch status {
	case "", "all", "available", "disabled", "unavailable", "attention", "quota_exhausted", "quota_restricted", "usable", "quota_unknown":
	default:
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
	rows, err := tx.QueryContext(ctx, "SELECT id,auth_index,status,"+accountDisplayStatusSQL+","+accountStatusObservedAtSQL+",COALESCE(quota,''),COALESCE(observed_at,'') FROM accounts WHERE status!='removed' ORDER BY id")
	if err != nil {
		return page, err
	}
	at, needle := time.Now(), strings.ToLower(search)
	filtered := []Account{}
	for rows.Next() {
		var account Account
		var raw string
		if err = rows.Scan(&account.ID, &account.AuthIndex, &account.Status, &account.DisplayStatus, &account.StatusObservedAt, &raw, &account.ObservedAt); err != nil {
			rows.Close()
			return page, err
		}
		page.Total++
		if !strings.Contains(strings.ToLower(account.ID), needle) {
			continue
		}
		known, usable := quotaState([]byte(raw), at)
		match := status == "usable" && account.Status == "available" && usable
		if status == "quota_unknown" {
			match = account.Status != "disabled" && account.DisplayStatus != "quota_exhausted" && account.DisplayStatus != "quota_restricted" && (!known || account.Status != "available")
		}
		if status == "" || status == "all" || status == "available" && account.DisplayStatus == "available" || status == "disabled" && account.Status == "disabled" || status == "unavailable" && account.Status != "available" && account.Status != "disabled" || status == "attention" && account.DisplayStatus != "available" && account.DisplayStatus != "disabled" || status == "quota_exhausted" && account.DisplayStatus == status || status == "quota_restricted" && account.DisplayStatus == status {
			match = true
		}
		if !match {
			continue
		}
		if raw != "" {
			account.Quota = json.RawMessage(raw)
		}
		filtered = append(filtered, account)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	order := ""
	if len(orders) > 0 {
		order = orders[0]
	}
	amount := func(account Account) float64 {
		var snapshot quota.Snapshot
		if json.Unmarshal(account.Quota, &snapshot) != nil {
			return math.Inf(1)
		}
		window := snapshot.FiveHour
		if order == "weekly" {
			window = snapshot.Weekly
		}
		if window == nil || window.Remaining == nil || window.Remaining.Unit != "percent" || math.IsNaN(window.Remaining.Value) || math.IsInf(window.Remaining.Value, 0) || window.Remaining.Value < 0 || window.Remaining.Value > 100 {
			return math.Inf(1)
		}
		return window.Remaining.Value
	}
	if order == "five_hour" || order == "weekly" {
		sort.SliceStable(filtered, func(i, j int) bool { return amount(filtered[i]) < amount(filtered[j]) })
	}
	if order == "attention" {
		weight := func(account Account) int {
			known, _ := quotaState(account.Quota, at)
			if account.Status != "disabled" && account.DisplayStatus != "quota_exhausted" && account.DisplayStatus != "quota_restricted" && (!known || account.Status != "available") {
				return 0
			}
			switch account.DisplayStatus {
			case "available":
				return 2
			case "disabled":
				return 3
			case "quota_exhausted", "quota_restricted":
				return 1
			default:
				return 0
			}
		}
		sort.SliceStable(filtered, func(i, j int) bool { return weight(filtered[i]) < weight(filtered[j]) })
	}
	page.FilterTotal = len(filtered)
	if after != "" {
		found := false
		page.Offset = len(filtered)
		for index, account := range filtered {
			if account.ID == after {
				page.Offset = index + 1
				found = true
				break
			}
		}
		if !found {
			return page, ErrViewChanged
		}
	}
	end := min(page.Offset+limit, len(filtered))
	page.Accounts = append(page.Accounts, filtered[page.Offset:end]...)
	if end < len(filtered) && len(page.Accounts) > 0 {
		page.Next = page.Accounts[len(page.Accounts)-1].ID
	}
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return page, err
	}
	for index := range page.Accounts {
		page.Accounts[index].PendingQuery, err = readPendingQuery(ctx, tx, page.Accounts[index].ID, settings.Config)
		if err != nil {
			return page, err
		}
	}
	return page, tx.Commit()
}
