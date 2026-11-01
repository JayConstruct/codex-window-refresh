package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/v2/quota"
)

// Merge response evidence in the same transaction as finishing the send. This
// avoids an extra query and prevents a replaced binding or older response from
// overwriting a newer account observation.
func mergeWakeResponseQuota(ctx context.Context, tx *sql.Tx, id string, snapshot quota.Snapshot, at time.Time) (bool, error) {
	job, err := scanWake(tx.QueryRowContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE id=? AND phase='sending'", id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if snapshot.Binding != job.Account.Binding || snapshot.ObservedAt.After(at) || snapshot.ReadStartedAt.Before(parseWakeTime(job.UpdatedAt)) {
		return false, nil
	}
	if snapshot.Source != "cpa_response_headers" && snapshot.Source != "cpa_websocket_quota" {
		return false, nil
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(a.quota,'') FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=? AND b.binding=? AND a.status NOT IN ('removed','binding_unavailable')`, job.AccountID, job.Account.AuthIndex, snapshot.Binding).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	merged, changed, err := quota.Merge([]byte(old), raw)
	if err != nil {
		return false, err
	}
	var result quota.Snapshot
	if err = json.Unmarshal(merged, &result); err != nil {
		return false, err
	}
	if changed {
		if _, err = tx.ExecContext(ctx, "UPDATE accounts SET quota=?,observed_at=? WHERE id=?", string(merged), queryStamp(result.ObservedAt), job.AccountID); err != nil {
			return false, err
		}
	}
	return wakeWindowVerified(result, job.Account.Binding, job.UpdatedAt, at), nil
}

func parseWakeTime(stamp string) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, stamp)
	return at
}
