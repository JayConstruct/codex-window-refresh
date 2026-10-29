package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

const wakeQuotaMaxAge = 30 * time.Second

type wakeBaseline struct{ Binding, FiveHour, Weekly, Boot string }

func readWakeBaseline(ctx context.Context, tx *sql.Tx, account Account) (wakeBaseline, error) {
	baseline := wakeBaseline{Binding: account.Binding}
	err := tx.QueryRowContext(ctx, "SELECT binding,five_hour_reset,weekly_reset,boot FROM wake_baselines WHERE account_id=?", account.ID).Scan(&baseline.Binding, &baseline.FiveHour, &baseline.Weekly, &baseline.Boot)
	if errors.Is(err, sql.ErrNoRows) {
		return wakeBaseline{Binding: account.Binding}, nil
	}
	if err != nil {
		return baseline, err
	}
	if baseline.Binding != account.Binding {
		return wakeBaseline{Binding: account.Binding}, nil
	}
	return baseline, nil
}

func wakeInventoryReady(ctx context.Context, tx *sql.Tx, at time.Time) (bool, error) {
	var status, stamp string
	if err := tx.QueryRowContext(ctx, "SELECT status,last_success_at FROM inventory_state WHERE singleton=1").Scan(&status, &stamp); err != nil {
		return false, err
	}
	observed, err := time.Parse(time.RFC3339Nano, stamp)
	return status == "ready" && err == nil && !observed.After(at.Add(5*time.Second)) && at.Sub(observed) <= 90*time.Second, nil
}

func wakeFieldStamp(snapshot quota.Snapshot, name string) quota.Stamp {
	stamp, ok := snapshot.Fields[name]
	if ok {
		return stamp
	}
	started := snapshot.ReadStartedAt
	if started.IsZero() {
		started = snapshot.ObservedAt
	}
	return quota.Stamp{Source: snapshot.Source, ObservedAt: snapshot.ObservedAt, ReadStartedAt: started}
}

func wakeFresh(snapshot quota.Snapshot, binding, gate string, at time.Time) bool {
	if snapshot.Binding != binding || snapshot.Permission == nil || snapshot.FiveHour == nil || snapshot.Weekly == nil {
		return false
	}
	boundary, _ := time.Parse(time.RFC3339Nano, gate)
	for _, name := range []string{"permission", "five_hour", "weekly"} {
		stamp := wakeFieldStamp(snapshot, name)
		if stamp.ObservedAt.IsZero() || stamp.ObservedAt.After(at) || at.Sub(stamp.ObservedAt) > wakeQuotaMaxAge || stamp.ReadStartedAt.IsZero() || stamp.ReadStartedAt.After(stamp.ObservedAt) || stamp.ReadStartedAt.Before(boundary) {
			return false
		}
	}
	return true
}

func wakeSnapshotGate(snapshot quota.Snapshot) string {
	var earliest time.Time
	for _, name := range []string{"permission", "five_hour", "weekly"} {
		stamp := wakeFieldStamp(snapshot, name)
		if earliest.IsZero() || stamp.ReadStartedAt.Before(earliest) {
			earliest = stamp.ReadStartedAt
		}
	}
	if earliest.IsZero() {
		return ""
	}
	return queryStamp(earliest)
}

func wakeQuotaDecision(snapshot quota.Snapshot, baseline wakeBaseline, job WakeJob, at time.Time) string {
	if !wakeFresh(snapshot, job.Account.Binding, job.QueryGate, at) {
		return "quota_unknown"
	}
	short, weekly := *snapshot.FiveHour, *snapshot.Weekly
	short.ConfirmedAt = wakeFieldStamp(snapshot, "five_hour").ObservedAt
	if short.State == core.WindowActive && short.ResetAt.After(at) {
		if job.Reason == "weekly_reset" {
			return "weekly_reset_conflict"
		}
		return "active_window"
	}
	if *snapshot.Permission != core.PermissionAllowed {
		return "quota_blocked"
	}
	for _, window := range []core.WindowEvidence{short, weekly} {
		if window.Remaining == nil || math.IsNaN(window.Remaining.Value) || math.IsInf(window.Remaining.Value, 0) || window.Remaining.Value <= 0 {
			return "quota_blocked"
		}
	}
	reset, _ := time.Parse(time.RFC3339Nano, baseline.FiveHour)
	full := core.FullFiveHourAfterReset(short, reset, at) || job.Reason == "manual" && core.FullFiveHourPlaceholder(short)
	if !reset.IsZero() && reset.After(at) && short.State == core.WindowUnknown {
		return "active_window"
	}
	if short.State != core.WindowInactive && short.State != core.WindowExpired && !full {
		return "quota_unknown"
	}
	if short.State == core.WindowExpired && (short.ResetAt.IsZero() || short.ResetAt.After(at)) {
		return "quota_unknown"
	}
	if weekly.State == core.WindowExpired {
		return "quota_unknown"
	}
	if weekly.State != core.WindowActive && weekly.State != core.WindowInactive && weekly.State != core.WindowUnknown {
		return "quota_unknown"
	}
	if job.Reason == "weekly_reset" {
		boundary, parseErr := time.Parse(time.RFC3339Nano, job.Cycle)
		if parseErr != nil {
			boundary, _ = time.Parse(time.RFC3339Nano, baseline.Weekly)
		}
		stamp := wakeFieldStamp(snapshot, "weekly").ObservedAt
		if boundary.IsZero() || boundary.After(at) || stamp.Before(boundary) || weekly.ResetAt.IsZero() || !weekly.ResetAt.After(boundary) {
			return "weekly_reset_unconfirmed"
		}
	}
	return ""
}

func wakeActivityHold(ctx context.Context, tx *sql.Tx, accountID string, at time.Time) (string, error) {
	cursor, err := readCursor(tx.QueryRowContext(ctx, cursorSQL))
	if err != nil {
		return "", err
	}
	last, parseErr := time.Parse(time.RFC3339Nano, cursor.LastSeen)
	if !cursor.Connected || cursor.Status != "observing" || parseErr != nil || last.After(at.Add(5*time.Second)) || at.Sub(last) > 5*time.Second {
		return "activity_unknown", nil
	}
	var busy int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_requests WHERE phase='unknown' OR (phase='active' AND (account_id=? OR account_id=''))", accountID).Scan(&busy)
	if err != nil {
		return "", err
	}
	if busy > 0 {
		return "account_busy", nil
	}
	return "", nil
}

func wakeCapacity(ctx context.Context, tx *sql.Tx, config policy.WakeConfig, at time.Time, excludeID string) (int, int, error) {
	var users int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_users u WHERE last_activity>=? OR EXISTS(SELECT 1 FROM activity_requests r WHERE r.identity=u.identity AND r.phase='active')", queryStamp(at.Add(-30*time.Minute))).Scan(&users); err != nil {
		return 0, 0, err
	}
	target := min(config.MaxAccounts, int(math.Ceil(float64(users)*config.AccountsPerUser)))
	_, _, _, dailyReady := config.DailyBounds(at)
	if config.Enabled && config.DailyEnabled && dailyReady && config.InAllowedHours(at) {
		target = max(target, config.DailyTarget)
	}
	capacity := 0
	active := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT COALESCE(a.quota,''),a.id FROM accounts a WHERE a.status='available'")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var raw, id string
		if err = rows.Scan(&raw, &id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		_, usable := quotaState([]byte(raw), at)
		if usable {
			capacity++
			active[id] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	var reserved int
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT w.account_id FROM wake_jobs w JOIN accounts a ON a.id=w.account_id JOIN account_bindings b ON b.account_id=a.id WHERE w.id!=? AND w.binding=b.binding AND a.status='available' AND w.phase IN ('prepared','sending','verifying','unknown') AND w.hold_until>?`, excludeID, queryStamp(at))
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if !active[id] {
			reserved++
		}
	}
	err = rows.Err()
	rows.Close()
	return max(0, target-capacity-reserved), users, err
}

func wakeModelBusy(ctx context.Context, tx *sql.Tx, job WakeJob) (string, error) {
	var physical, account int
	err := tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM prompt_generations WHERE phase='running')+(SELECT count(*) FROM wake_jobs WHERE id!=? AND phase IN ('prepared','sending')),
 (SELECT count(*) FROM prompt_generations WHERE account_id=? AND (binding=? OR provider='antigravity') AND phase IN ('queued','running','unknown'))+(SELECT count(*) FROM reset_attempts WHERE account_id=? AND binding=? AND phase IN ('sending','unknown','verifying'))`, job.ID, job.AccountID, job.Account.Binding, job.AccountID, job.Account.Binding).Scan(&physical, &account)
	if err != nil {
		return "", err
	}
	if account > 0 {
		return "account_busy", nil
	}
	if physical > 0 {
		return "model_busy", nil
	}
	return "", nil
}
