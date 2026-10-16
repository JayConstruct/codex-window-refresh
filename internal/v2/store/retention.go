package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Retention struct {
	HistoryAge   time.Duration
	TerminalJobs int
}

func DefaultRetention() Retention {
	return Retention{HistoryAge: 7 * 24 * time.Hour, TerminalJobs: 10000}
}

// Prune performs a bounded transaction. It never evicts accepted/running jobs or
// the receipt protecting a still-active operation, and never declares a missing
// completion successful merely because time elapsed.
func (s *Store) Prune(ctx context.Context, at time.Time, p Retention) error {
	if p.HistoryAge <= 0 || p.TerminalJobs < 1 {
		return errors.New("invalid_retention_policy")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		cutoff := at.UTC().Add(-p.HistoryAge).Format("2006-01-02T15:04:05.000000000Z")
		rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs WHERE phase IN ('completed','failed','cancelled') AND (updated_at<? OR id IN(SELECT id FROM jobs WHERE phase IN ('completed','failed','cancelled') ORDER BY updated_at DESC,id DESC LIMIT -1 OFFSET ?)) ORDER BY updated_at,id LIMIT 256`, cutoff, p.TerminalJobs)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err = tx.ExecContext(ctx, "DELETE FROM job_items WHERE job_id=?", id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM jobs WHERE id=?", id); err != nil {
				return err
			}
		}
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{query: "DELETE FROM query_runs WHERE id IN(SELECT r.id FROM query_runs r WHERE r.phase!='running' AND NOT EXISTS(SELECT 1 FROM job_items i WHERE i.run_id=r.id) LIMIT 256)"},
			{query: "DELETE FROM activity_requests WHERE rowid IN(SELECT rowid FROM activity_requests WHERE phase='completed' AND last_event_at<? LIMIT 256)", args: []any{at.UTC().Add(-24 * time.Hour).Format("2006-01-02T15:04:05.000000000Z")}},
			{query: "DELETE FROM activity_requests WHERE rowid IN(SELECT rowid FROM activity_requests WHERE phase='completed' ORDER BY last_event_at DESC LIMIT -1 OFFSET 8192)"},
		} {
			_, err = tx.ExecContext(ctx, statement.query, statement.args...)
			if err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE activity_requests SET phase='unknown' WHERE phase='active' AND last_event_at<?", at.UTC().Add(-24*time.Hour).Format("2006-01-02T15:04:05.000000000Z"))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if _, err = tx.ExecContext(ctx, "UPDATE event_state SET status='gap',reason='missing_completion' WHERE singleton=1"); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM activity_users WHERE last_activity<? AND NOT EXISTS(SELECT 1 FROM activity_requests r WHERE r.identity=activity_users.identity AND r.phase='active')", at.UTC().Add(-30*time.Minute).Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM job_receipts WHERE idempotency_key IN(SELECT r.idempotency_key FROM job_receipts r WHERE expires_at<? AND NOT EXISTS(SELECT 1 FROM jobs j WHERE j.id=r.job_id AND j.phase IN ('queued','running','blocked')) LIMIT 256)", at.Unix())
		return err
	})
}
