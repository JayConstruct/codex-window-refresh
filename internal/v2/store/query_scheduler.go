package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

type queryBaseline struct {
	Binding  string
	FiveHour string
	Weekly   string
}

type queryTimer struct {
	Binding   string
	Cycle     string
	JobID     string
	Phase     string
	Attempts  int
	UpdatedAt string
}

func timerReady(ctx context.Context, tx *sql.Tx, account Account, trigger, cycle string, at time.Time, config policy.QueryConfig) (queryTimer, bool, error) {
	var timer queryTimer
	err := tx.QueryRowContext(ctx, "SELECT binding,cycle,job_id,phase,attempts,updated_at FROM query_timers WHERE account_id=? AND kind=?", account.ID, trigger).Scan(&timer.Binding, &timer.Cycle, &timer.JobID, &timer.Phase, &timer.Attempts, &timer.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (timer.Binding != account.Binding || timer.Cycle != cycle) {
		return queryTimer{}, true, nil
	}
	if err != nil {
		return timer, false, err
	}
	switch timer.Phase {
	case "queued", "running", "blocked":
		return timer, false, nil
	case "completed":
		if trigger != "periodic" {
			return timer, false, nil
		}
		updated, err := time.Parse(time.RFC3339Nano, timer.UpdatedAt)
		if err != nil {
			return timer, false, err
		}
		return timer, !at.Before(updated.Add(time.Duration(config.Interval) * time.Second)), nil
	case "failed":
		updated, err := time.Parse(time.RFC3339Nano, timer.UpdatedAt)
		if err != nil {
			return timer, false, err
		}
		delay := min(15*time.Minute, 30*time.Second*time.Duration(1<<min(max(timer.Attempts-1, 0), 5)))
		return timer, !at.Before(updated.Add(delay)), nil
	case "cancelled":
		return timer, true, nil
	}
	return timer, false, errors.New("invalid_query_timer")
}

func enqueueAutomatic(ctx context.Context, tx *sql.Tx, account Account, plan QueryPlan, at time.Time) (string, string, bool, error) {
	delay, err := queryDelay(plan)
	if err != nil {
		return "", "", false, err
	}
	due := at.Add(delay)
	var jobID, scheduled string
	err = tx.QueryRowContext(ctx, `SELECT i.job_id,q.scheduled_at FROM job_items i JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id JOIN jobs j ON j.id=i.job_id
 WHERE i.account_id=? AND q.binding=? AND q.auth_index=? AND i.phase IN ('queued','running') AND j.phase='running' AND (i.phase='running' OR q.scheduled_at<=?) ORDER BY CASE WHEN i.phase='running' THEN 0 ELSE 1 END,q.scheduled_at LIMIT 1`, account.ID, account.Binding, account.AuthIndex, queryStamp(due)).Scan(&jobID, &scheduled)
	if err == nil {
		return jobID, scheduled, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", false, err
	}
	var pending, jobs int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM job_items WHERE phase IN ('queued','running')),(SELECT count(*) FROM jobs WHERE phase IN ('queued','running','blocked'))").Scan(&pending, &jobs); err != nil {
		return "", "", false, err
	}
	if pending >= MaxPendingItems || jobs >= MaxPendingItems {
		return "", "", false, ErrBusy
	}
	jobID = ID()
	stamp, key := queryStamp(at), "auto:"+jobID
	digest := sha256.Sum256([]byte(plan.Trigger + "\x00" + account.ID + "\x00" + account.Binding))
	hash := hex.EncodeToString(digest[:])
	if _, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,expanded,created_at,updated_at) VALUES(?,?,?,?,'running',1,?,?)", jobID, key, hash, account.ID, stamp, stamp); err != nil {
		return "", "", false, err
	}
	if err = insertQueryPlan(ctx, tx, jobID, plan); err != nil {
		return "", "", false, err
	}
	if err = insertQueryItemScheduled(ctx, tx, jobID, account, due); err != nil {
		return "", "", false, err
	}
	err = tx.QueryRowContext(ctx, "SELECT scheduled_at FROM query_schedule WHERE job_id=? AND account_id=?", jobID, account.ID).Scan(&scheduled)
	return jobID, scheduled, true, err
}

func updateQueryBaseline(ctx context.Context, tx *sql.Tx, account Account, snapshot quota.Snapshot) (queryBaseline, error) {
	baseline := queryBaseline{Binding: account.Binding}
	err := tx.QueryRowContext(ctx, "SELECT binding,five_hour_reset,weekly_reset FROM query_baselines WHERE account_id=?", account.ID).Scan(&baseline.Binding, &baseline.FiveHour, &baseline.Weekly)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return baseline, err
	}
	if baseline.Binding != account.Binding {
		baseline = queryBaseline{Binding: account.Binding}
	}
	previous := baseline
	if snapshot.Binding == account.Binding {
		for _, field := range []struct {
			window *core.WindowEvidence
			target *string
		}{{snapshot.FiveHour, &baseline.FiveHour}, {snapshot.Weekly, &baseline.Weekly}} {
			if field.window != nil && !field.window.ResetAt.IsZero() && (field.window.State == core.WindowActive || field.window.State == core.WindowExpired) {
				*field.target = queryStamp(field.window.ResetAt)
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) || baseline != previous {
		_, err = tx.ExecContext(ctx, "INSERT INTO query_baselines VALUES(?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET binding=excluded.binding,five_hour_reset=excluded.five_hour_reset,weekly_reset=excluded.weekly_reset", account.ID, baseline.Binding, baseline.FiveHour, baseline.Weekly)
		if err != nil {
			return baseline, err
		}
	}
	return baseline, nil
}

func (st *Store) ScheduleQueries(ctx context.Context, at time.Time) (int, error) {
	created := 0
	err := st.write(ctx, func(tx *sql.Tx) error {
		settings, err := readQuerySettings(ctx, tx)
		if err != nil {
			return err
		}
		config := settings.Config
		if !config.Enabled("initial_sync") {
			return nil
		}
		var status, synced, cursor string
		if err = tx.QueryRowContext(ctx, "SELECT status,last_success_at FROM inventory_state WHERE singleton=1").Scan(&status, &synced); err != nil {
			return err
		}
		verified, parseErr := time.Parse(time.RFC3339Nano, synced)
		if status != "ready" || parseErr != nil || verified.After(at.Add(5*time.Second)) || at.Sub(verified) > 90*time.Second {
			return nil
		}
		if !config.Allowed(at) {
			return nil
		}
		if err = tx.QueryRowContext(ctx, "SELECT account_id FROM query_scheduler_cursor WHERE singleton=1").Scan(&cursor); err != nil {
			return err
		}
		accounts := []Account{}
		for attempt := 0; attempt < 2; attempt++ {
			rows, err := tx.QueryContext(ctx, `SELECT a.id,a.auth_index,a.status,COALESCE(a.quota,''),b.binding FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id>? AND b.binding!='' AND a.auth_index!='' AND a.status NOT IN ('removed','disabled','binding_unavailable') ORDER BY a.id LIMIT 256`, cursor)
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
			var snapshot quota.Snapshot
			_ = json.Unmarshal(account.Quota, &snapshot)
			baseline, err := updateQueryBaseline(ctx, tx, account, snapshot)
			if err != nil {
				return err
			}
			for _, entry := range []struct{ kind, cycle string }{{"weekly_expiry", baseline.Weekly}, {"five_hour_expiry", baseline.FiveHour}, {"periodic", "periodic"}, {"initial_sync", "initial"}} {
				if created >= 64 {
					break
				}
				if !config.Enabled(entry.kind) {
					continue
				}
				if entry.kind == "initial_sync" && snapshot.Binding == account.Binding && snapshot.Permission != nil && (!config.FiveHour || snapshot.FiveHour != nil) && (!config.Weekly || snapshot.Weekly != nil) {
					continue
				}
				if entry.kind == "five_hour_expiry" || entry.kind == "weekly_expiry" {
					boundary, err := time.Parse(time.RFC3339Nano, entry.cycle)
					if err != nil || at.Before(boundary) {
						continue
					}
				}
				timer, ready, err := timerReady(ctx, tx, account, entry.kind, entry.cycle, at, config)
				if err != nil {
					return err
				}
				if !ready {
					continue
				}
				plan := QueryPlan{Trigger: entry.kind, Automatic: true, Min: config.Delay.Min, Max: config.Delay.Max}
				jobID, scheduled, fresh, err := enqueueAutomatic(ctx, tx, account, plan, at)
				if err != nil {
					return err
				}
				attempts := 1
				if timer.Phase == "failed" {
					attempts = timer.Attempts + 1
				}
				var phase string
				if err = tx.QueryRowContext(ctx, "SELECT phase FROM jobs WHERE id=?", jobID).Scan(&phase); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO query_timers VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id,kind) DO UPDATE SET binding=excluded.binding,cycle=excluded.cycle,next_at=excluded.next_at,job_id=excluded.job_id,phase=excluded.phase,attempts=excluded.attempts,updated_at=excluded.updated_at`, account.ID, entry.kind, account.Binding, entry.cycle, scheduled, jobID, phase, attempts, queryStamp(at)); err != nil {
					return err
				}
				if fresh {
					created++
				}
			}
			if created >= 64 {
				break
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE query_scheduler_cursor SET account_id=? WHERE singleton=1", cursor)
		return err
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}
