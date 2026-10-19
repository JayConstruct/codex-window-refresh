package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/quota"
)

func wakePriority(reason string) int {
	switch reason {
	case "weekly_reset":
		return 0
	case "manual":
		return 1
	case "five_hour_reset":
		return 2
	default:
		return 3
	}
}

func enqueueWake(ctx context.Context, tx *sql.Tx, account Account, reason, cycle string, at, due time.Time, gate string) (bool, error) {
	var previousCycle, previousPhase string
	err := tx.QueryRowContext(ctx, "SELECT c.cycle,COALESCE(w.phase,'') FROM wake_cycles c LEFT JOIN wake_jobs w ON w.id=c.job_id WHERE c.account_id=? AND c.binding=? AND c.reason=?", account.ID, account.Binding, reason).Scan(&previousCycle, &previousPhase)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && previousCycle == cycle && previousPhase != "cancelled" {
		return false, nil
	}
	var id, existingReason, phase string
	err = tx.QueryRowContext(ctx, "SELECT id,reason,phase FROM wake_jobs WHERE account_id=? AND binding=? AND phase IN ('queued','checking','prepared','sending','verifying','unknown')", account.ID, account.Binding).Scan(&id, &existingReason, &phase)
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return false, err
	}
	if !fresh {
		if phase != "queued" && phase != "checking" {
			return false, nil
		}
		if existingReason != "manual" && wakePriority(reason) < wakePriority(existingReason) {
			if _, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET reason=?,cycle=?,updated_at=? WHERE id=?", reason, cycle, queryStamp(at), id); err != nil {
				return false, err
			}
		}
	} else {
		var total, pending int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(phase IN ('queued','checking','prepared','sending','verifying','unknown')),0) FROM wake_jobs").Scan(&total, &pending); err != nil {
			return false, err
		}
		if total >= 10000 || pending >= 4096 {
			return false, ErrBusy
		}
		id = ID()
		_, err = tx.ExecContext(ctx, "INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at,query_gate) VALUES(?,?,?,?,?,?,'queued',?,?,?,?)", id, account.ID, account.AuthIndex, account.Binding, reason, cycle, queryStamp(due), queryStamp(at), queryStamp(at), gate)
		if err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO wake_cycles VALUES(?,?,?,?,?) ON CONFLICT(account_id,binding,reason) DO UPDATE SET cycle=excluded.cycle,job_id=excluded.job_id", account.ID, account.Binding, reason, cycle, id)
	return fresh, err
}

func (st *Store) ScheduleWakes(ctx context.Context, at time.Time, boot string) (int, error) {
	created := 0
	err := st.write(ctx, func(tx *sql.Tx) error {
		if err := verifyWakes(ctx, tx, at); err != nil {
			return err
		}
		settings, err := readWakeSettings(ctx, tx)
		if err != nil {
			return err
		}
		ready, err := wakeInventoryReady(ctx, tx, at)
		if err != nil || !ready {
			return err
		}
		var cursor string
		if err = tx.QueryRowContext(ctx, "SELECT account_id FROM wake_cursor WHERE singleton=1").Scan(&cursor); err != nil {
			return err
		}
		accounts := []Account{}
		for attempt := 0; attempt < 2; attempt++ {
			rows, err := tx.QueryContext(ctx, "SELECT a.id,a.auth_index,a.status,COALESCE(a.quota,''),b.binding FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id>? AND a.status='available' AND a.auth_index!='' AND b.binding!='' ORDER BY a.id LIMIT 256", cursor)
			if err != nil {
				return err
			}
			for rows.Next() {
				var account Account
				var raw string
				if err = rows.Scan(&account.ID, &account.AuthIndex, &account.Status, &raw, &account.Binding); err != nil {
					rows.Close()
					return err
				}
				account.Provider = "codex"
				account.Quota = json.RawMessage(raw)
				accounts = append(accounts, account)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(accounts) > 0 || cursor == "" {
				break
			}
			cursor = ""
		}
		for _, account := range accounts {
			cursor = account.ID
			baseline, err := readWakeBaseline(ctx, tx, account)
			if err != nil {
				return err
			}
			previous := baseline
			var snapshot quota.Snapshot
			_ = json.Unmarshal(account.Quota, &snapshot)
			fresh := wakeFresh(snapshot, account.Binding, "", at)
			if snapshot.Binding == account.Binding {
				for _, field := range []struct {
					window *core.WindowEvidence
					target *string
				}{{snapshot.FiveHour, &baseline.FiveHour}, {snapshot.Weekly, &baseline.Weekly}} {
					if field.window != nil && (field.window.State == core.WindowActive || field.window.State == core.WindowExpired) && !field.window.ResetAt.IsZero() {
						stamp := queryStamp(field.window.ResetAt)
						if *field.target == "" || stamp > *field.target {
							*field.target = stamp
						}
					}
				}
			}
			startup := baseline.Boot != boot
			if fresh {
				baseline.Boot = boot
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO wake_baselines VALUES(?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET binding=excluded.binding,five_hour_reset=excluded.five_hour_reset,weekly_reset=excluded.weekly_reset,boot=excluded.boot", account.ID, account.Binding, baseline.FiveHour, baseline.Weekly, baseline.Boot); err != nil {
				return err
			}
			if !settings.Config.Enabled {
				continue
			}
			gate := ""
			if fresh {
				gate = wakeSnapshotGate(snapshot)
			}
			for _, entry := range []struct{ reason, cycle string }{{"weekly_reset", previous.Weekly}, {"five_hour_reset", previous.FiveHour}, {"daily", ""}} {
				if !settings.Config.RuleEnabled(entry.reason) {
					continue
				}
				if entry.cycle == "" {
					if entry.reason == "weekly_reset" {
						entry.cycle = baseline.Weekly
					} else if entry.reason == "five_hour_reset" {
						entry.cycle = baseline.FiveHour
					}
				}
				due := at
				if entry.reason == "daily" {
					date, start, end, ready := settings.Config.DailyBounds(at)
					if !ready || !settings.Config.InAllowedHours(at) {
						continue
					}
					entry.cycle = date
					if at.Before(end) {
						span := int(end.Sub(start) / time.Second)
						delay, err := queryDelay(QueryPlan{Max: span})
						if err != nil {
							return err
						}
						due = start.Add(delay)
						if due.Before(at) {
							due = at
						}
					}
				} else if entry.cycle != "" {
					boundary, err := time.Parse(time.RFC3339Nano, entry.cycle)
					if err != nil || boundary.After(at) {
						continue
					}
				} else {
					if fresh {
						continue
					}
					entry.cycle = "initial"
				}
				if startup && entry.reason != "daily" {
					delay, err := queryDelay(QueryPlan{Min: settings.Config.Delay.Min, Max: settings.Config.Delay.Max})
					if err != nil {
						return err
					}
					due = at.Add(delay)
				}
				made, err := enqueueWake(ctx, tx, account, entry.reason, entry.cycle, at, due, gate)
				if err != nil {
					return err
				}
				if made {
					created++
				}
				if created >= 64 {
					break
				}
			}
			if created >= 64 {
				break
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE wake_cursor SET account_id=? WHERE singleton=1", cursor)
		return err
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}

func enqueueWakeRead(ctx context.Context, tx *sql.Tx, job *WakeJob, at time.Time) (bool, error) {
	if job.QueryGate == "" {
		job.QueryGate = queryStamp(at)
	}
	if job.QueryJobID != "" {
		var phase string
		err := tx.QueryRowContext(ctx, "SELECT phase FROM job_items WHERE job_id=? AND account_id=?", job.QueryJobID, job.AccountID).Scan(&phase)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if phase == "queued" || phase == "running" {
			return false, nil
		}
		job.QueryJobID = ""
	}
	var running string
	err := tx.QueryRowContext(ctx, "SELECT r.id FROM query_runs r JOIN query_run_bindings b ON b.run_id=r.id WHERE r.account_id=? AND r.auth_index=? AND b.binding=? AND r.phase='running' AND r.started_at<? LIMIT 1", job.AccountID, job.Account.AuthIndex, job.Account.Binding, job.QueryGate).Scan(&running)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT i.job_id FROM job_items i JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id LEFT JOIN query_runs r ON r.id=i.run_id WHERE i.account_id=? AND q.binding=? AND q.auth_index=? AND (i.phase='queued' OR (i.phase='running' AND r.started_at>=?)) ORDER BY q.scheduled_at LIMIT 1`, job.AccountID, job.Account.Binding, job.Account.AuthIndex, job.QueryGate).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		settings, err := readQuerySettings(ctx, tx)
		if err != nil {
			return false, err
		}
		var pending int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM job_items WHERE phase IN ('queued','running')").Scan(&pending); err != nil {
			return false, err
		}
		if pending >= MaxPendingItems {
			return false, ErrBusy
		}
		id = ID()
		stamp := queryStamp(at)
		if _, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,expanded,created_at,updated_at) VALUES(?,?,?,?,'running',1,?,?)", id, "wake-query:"+id, id, job.AccountID, stamp, stamp); err != nil {
			return false, err
		}
		plan := QueryPlan{Trigger: "wake_preflight", Min: settings.Config.Delay.Min, Max: settings.Config.Delay.Max}
		if job.Phase == "verifying" || job.Phase == "unknown" {
			plan.Trigger = "wake_verification"
		}
		if err = insertQueryPlan(ctx, tx, id, plan); err != nil {
			return false, err
		}
		if err = insertQueryItem(ctx, tx, id, job.Account, plan, at); err != nil {
			return false, err
		}
	}
	job.QueryJobID = id
	_, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET query_job_id=?,query_gate=?,updated_at=? WHERE id=?", id, job.QueryGate, queryStamp(at), job.ID)
	return true, err
}

func verifyWakes(ctx context.Context, tx *sql.Tx, at time.Time) error {
	rows, err := tx.QueryContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE phase IN ('verifying','unknown') AND next_check_at<=? ORDER BY updated_at,id LIMIT 32", queryStamp(at))
	if err != nil {
		return err
	}
	jobs := []WakeJob{}
	for rows.Next() {
		job, scanErr := scanWake(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err = updateWake(ctx, tx, job, job.Phase, job.ErrorCode, job.HoldReason, at); err != nil {
			return err
		}
		account, err := wakeBoundAccount(ctx, tx, job, at)
		if err != nil && !errors.Is(err, ErrWakeAccount) {
			return err
		}
		if errors.Is(err, ErrWakeAccount) {
			if err = updateWake(ctx, tx, job, job.Phase, job.ErrorCode, WakeAccountErrorCode(err), at); err != nil {
				return err
			}
			continue
		}
		var snapshot quota.Snapshot
		_ = json.Unmarshal(account.Quota, &snapshot)
		if wakeFresh(snapshot, account.Binding, job.QueryGate, at) && snapshot.FiveHour.State == core.WindowActive && snapshot.FiveHour.ResetAt.After(at) {
			if err = updateWake(ctx, tx, job, "completed", "window_verified", "", at); err != nil {
				return err
			}
			continue
		}
		if scheduled, parseErr := time.Parse(time.RFC3339Nano, job.ScheduledAt); parseErr == nil && scheduled.After(at) {
			continue
		}
		if _, err = enqueueWakeRead(ctx, tx, &job, at); err != nil {
			if errors.Is(err, ErrBusy) {
				continue
			}
			return err
		}
		delay := min(15*time.Minute, 30*time.Second*time.Duration(1<<min(job.VerificationAttempts, 5)))
		if _, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET verification_attempts=verification_attempts+1,scheduled_at=?,hold_reason='verification_pending' WHERE id=?", queryStamp(at.Add(delay)), job.ID); err != nil {
			return err
		}
	}
	return nil
}
