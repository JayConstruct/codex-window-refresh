package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"time"

	"codex-window-refresh/internal/v2/policy"
)

type QueryPlan struct {
	Trigger   string
	Automatic bool
	Min       int
	Max       int
}

type PendingQuery struct {
	JobID       string `json:"job_id"`
	Phase       string `json:"phase"`
	ScheduledAt string `json:"scheduled_at"`
	Trigger     string `json:"trigger"`
	HoldReason  string `json:"hold_reason,omitempty"`
}

func queryStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func queryDelay(plan QueryPlan) (time.Duration, error) {
	if plan.Min < 0 || plan.Max < plan.Min || plan.Max > 86400 {
		return 0, policy.ErrInvalid
	}
	span, err := rand.Int(rand.Reader, big.NewInt(int64(plan.Max-plan.Min+1)))
	if err != nil {
		return 0, err
	}
	return time.Duration(int64(plan.Min)+span.Int64()) * time.Second, nil
}

func insertQueryPlan(ctx context.Context, tx *sql.Tx, jobID string, plan QueryPlan) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO query_plans VALUES(?,?,?,?,?)", jobID, plan.Trigger, plan.Automatic, plan.Min, plan.Max)
	return err
}

func readQueryPlan(ctx context.Context, tx *sql.Tx, jobID, target string) (QueryPlan, error) {
	plan := QueryPlan{Trigger: "manual_batch"}
	if target != "" {
		plan.Trigger = "manual_single"
	}
	err := tx.QueryRowContext(ctx, "SELECT trigger,automatic,delay_min,delay_max FROM query_plans WHERE job_id=?", jobID).Scan(&plan.Trigger, &plan.Automatic, &plan.Min, &plan.Max)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, nil
	}
	return plan, err
}

func insertQueryItem(ctx context.Context, tx *sql.Tx, jobID string, account Account, plan QueryPlan, at time.Time) error {
	delay, err := queryDelay(plan)
	if err != nil {
		return err
	}
	return insertQueryItemScheduled(ctx, tx, jobID, account, at.Add(delay))
}

func insertQueryItemScheduled(ctx context.Context, tx *sql.Tx, jobID string, account Account, due time.Time) error {
	var err error
	if account.Binding == "" {
		err = tx.QueryRowContext(ctx, "SELECT binding FROM account_bindings WHERE account_id=? AND EXISTS(SELECT 1 FROM accounts WHERE id=? AND auth_index=?)", account.ID, account.ID, account.AuthIndex).Scan(&account.Binding)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	phase := "queued"
	var runID any
	var running string
	err = tx.QueryRowContext(ctx, "SELECT r.id FROM query_runs r JOIN query_run_bindings b ON b.run_id=r.id WHERE r.account_id=? AND r.auth_index=? AND b.binding=? AND r.phase='running'", account.ID, account.AuthIndex, account.Binding).Scan(&running)
	if err == nil {
		phase, runID = "running", running
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO job_items(job_id,account_id,phase,run_id) VALUES(?,?,?,?)", jobID, account.ID, phase, runID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO query_schedule VALUES(?,?,?,?,?)", jobID, account.ID, queryStamp(due), account.AuthIndex, account.Binding)
	return err
}

func finishQueryJobs(ctx context.Context, tx *sql.Tx, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE query_timers SET phase=(SELECT i.phase FROM job_items i WHERE i.job_id=query_timers.job_id AND i.account_id=query_timers.account_id),updated_at=?
 WHERE phase IN ('queued','running','blocked') AND EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=query_timers.job_id AND i.account_id=query_timers.account_id AND i.phase IN ('completed','failed','cancelled'))`, queryStamp(at)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET phase=CASE
 WHEN EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=jobs.id AND i.phase='failed') THEN 'failed'
 WHEN NOT EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=jobs.id AND i.phase='completed') AND EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=jobs.id AND i.phase='cancelled') THEN 'cancelled'
 ELSE 'completed' END,updated_at=?
 WHERE expanded=1 AND phase='running' AND NOT EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=jobs.id AND i.phase IN ('queued','running'))`, queryStamp(at))
	return err
}

func cancelDisabledQueries(ctx context.Context, tx *sql.Tx, config policy.QueryConfig) error {
	if _, err := tx.ExecContext(ctx, `WITH enabled(kind) AS (
 SELECT 'periodic' WHERE ? UNION ALL SELECT 'five_hour_expiry' WHERE ? UNION ALL SELECT 'weekly_expiry' WHERE ? UNION ALL SELECT 'initial_sync' WHERE ?)
 UPDATE query_plans SET trigger=(SELECT t.kind FROM query_timers t JOIN enabled e ON e.kind=t.kind
 WHERE t.job_id=query_plans.job_id AND t.phase IN ('queued','running','blocked')
 ORDER BY CASE t.kind WHEN 'weekly_expiry' THEN 0 WHEN 'five_hour_expiry' THEN 1 WHEN 'periodic' THEN 2 ELSE 3 END LIMIT 1)
 WHERE automatic=1 AND trigger NOT IN (SELECT kind FROM enabled)
 AND EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=query_plans.job_id AND i.phase='queued')
 AND EXISTS(SELECT 1 FROM query_timers t JOIN enabled e ON e.kind=t.kind WHERE t.job_id=query_plans.job_id AND t.phase IN ('queued','running','blocked'))`, config.Automatic, config.FiveHour, config.Weekly, config.Enabled("initial_sync")); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE job_items SET phase='cancelled',error_code='automatic_query_disabled'
 WHERE phase='queued' AND job_id IN(SELECT job_id FROM query_plans WHERE automatic=1 AND
 ((trigger='periodic' AND ?=0) OR (trigger='five_hour_expiry' AND ?=0) OR (trigger='weekly_expiry' AND ?=0) OR (trigger='initial_sync' AND ?=0)))`, config.Automatic, config.FiveHour, config.Weekly, config.Enabled("initial_sync"))
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return nil
	}
	return finishQueryJobs(ctx, tx, time.Now())
}

func (st *Store) ClaimAt(ctx context.Context, at time.Time) (Run, error) {
	run := Run{ID: ID()}
	claimed := false
	err := st.write(ctx, func(tx *sql.Tx) error {
		settings, err := readQuerySettings(ctx, tx)
		if err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE job_items SET phase='failed',error_code='account_binding_changed'
 WHERE phase='queued' AND EXISTS(SELECT 1 FROM query_schedule q LEFT JOIN accounts a ON a.id=q.account_id LEFT JOIN account_bindings b ON b.account_id=q.account_id
 WHERE q.job_id=job_items.job_id AND q.account_id=job_items.account_id AND
 (a.id IS NULL OR a.status='removed' OR a.auth_index!=q.auth_index OR (q.binding!='' AND COALESCE(b.binding,'')!=q.binding)))`)
		if err != nil {
			return err
		}
		if count, _ := changed.RowsAffected(); count > 0 {
			if err = finishQueryJobs(ctx, tx, at); err != nil {
				return err
			}
		}
		err = tx.QueryRowContext(ctx, `SELECT a.id,a.auth_index,a.status,COALESCE(b.binding,'')
 FROM job_items i JOIN accounts a ON a.id=i.account_id JOIN jobs j ON j.id=i.job_id
 LEFT JOIN account_bindings b ON b.account_id=a.id LEFT JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id
 LEFT JOIN query_plans p ON p.job_id=j.id
 WHERE i.phase='queued' AND j.phase='running' AND a.status!='removed' AND COALESCE(q.scheduled_at,j.created_at)<=?
 AND (COALESCE(p.automatic,0)=0 OR (?=1 AND
 ((p.trigger='periodic' AND ?=1) OR (p.trigger='five_hour_expiry' AND ?=1) OR (p.trigger='weekly_expiry' AND ?=1) OR (p.trigger='initial_sync' AND ?=1))))
 AND NOT EXISTS(SELECT 1 FROM query_runs r WHERE r.account_id=i.account_id AND r.phase='running')
 ORDER BY CASE WHEN COALESCE(p.trigger,'manual_single')='manual_single' THEN 0 ELSE 1 END,COALESCE(q.scheduled_at,j.created_at),j.created_at,i.account_id LIMIT 1`, queryStamp(at), settings.Config.Allowed(at), settings.Config.Automatic, settings.Config.FiveHour, settings.Config.Weekly, settings.Config.Enabled("initial_sync")).Scan(&run.Account.ID, &run.Account.AuthIndex, &run.Account.Status, &run.Account.Binding)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO query_runs(id,account_id,auth_index,phase,started_at) VALUES(?,?,?,'running',?)", run.ID, run.Account.ID, run.Account.AuthIndex, queryStamp(at)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO query_run_bindings VALUES(?,?)", run.ID, run.Account.Binding); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE job_items SET phase='running',run_id=? WHERE account_id=? AND phase='queued'", run.ID, run.Account.ID)
		claimed = err == nil
		return err
	})
	if err == nil && !claimed {
		err = ErrNotFound
	}
	return run, err
}

func readPendingQuery(ctx context.Context, tx *sql.Tx, accountID string, config policy.QueryConfig) (*PendingQuery, error) {
	query := &PendingQuery{}
	var automatic bool
	err := tx.QueryRowContext(ctx, `SELECT i.job_id,i.phase,COALESCE(q.scheduled_at,j.created_at),COALESCE(p.trigger,CASE WHEN j.target='' THEN 'manual_batch' ELSE 'manual_single' END),COALESCE(p.automatic,0)
 FROM job_items i JOIN jobs j ON j.id=i.job_id LEFT JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id LEFT JOIN query_plans p ON p.job_id=j.id
 WHERE i.account_id=? AND i.phase IN ('queued','running') ORDER BY CASE WHEN i.phase='running' THEN 0 ELSE 1 END,COALESCE(q.scheduled_at,j.created_at) LIMIT 1`, accountID).Scan(&query.JobID, &query.Phase, &query.ScheduledAt, &query.Trigger, &automatic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if query.Phase == "queued" && automatic && !config.Allowed(time.Now()) {
		query.HoldReason = "excluded_hours"
	}
	return query, nil
}
