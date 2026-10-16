package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"codex-window-refresh/internal/v2/events"
)

const MaxTrackedRequests = 4096
const MaxRecentIdentities = 8192

type EventCursor struct {
	Instance        string `json:"bridge_instance_id"`
	HostProcess     string `json:"host_process_id"`
	Sequence        uint64 `json:"sequence"`
	LossEpoch       uint64 `json:"loss_epoch"`
	Status          string `json:"status"`
	Reason          string `json:"reason"`
	Connected       bool   `json:"connected"`
	ConnectionError string `json:"connection_error,omitempty"`
	LastSeen        string `json:"last_seen,omitempty"`
}
type ActivitySummary struct {
	EventCursor
	RecentUsers      int  `json:"observed_recent_users"`
	ActiveRequests   int  `json:"observed_active_requests"`
	UnknownRequests  int  `json:"unknown_requests"`
	BaselineComplete bool `json:"baseline_complete"`
}
type Overview struct {
	Revision         uint64          `json:"view_revision"`
	AccountsRevision uint64          `json:"accounts_revision"`
	Activity         ActivitySummary `json:"activity"`
}

const cursorSQL = "SELECT bridge_instance,host_process,sequence,loss_epoch,status,reason,connected,connection_error,last_seen FROM event_state WHERE singleton=1"

type scanner interface{ Scan(...any) error }

func readCursor(row scanner) (EventCursor, error) {
	var c EventCursor
	err := row.Scan(&c.Instance, &c.HostProcess, &c.Sequence, &c.LossEpoch, &c.Status, &c.Reason, &c.Connected, &c.ConnectionError, &c.LastSeen)
	return c, err
}
func (s *Store) EventCursor(ctx context.Context) (EventCursor, error) {
	return readCursor(s.db.QueryRowContext(ctx, cursorSQL))
}
func (s *Store) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return o, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision,accounts_revision FROM view_state WHERE singleton=1").Scan(&o.Revision, &o.AccountsRevision); err != nil {
		return o, err
	}
	o.Activity.EventCursor, err = readCursor(tx.QueryRowContext(ctx, cursorSQL))
	if err != nil {
		return o, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_users u WHERE last_activity>=? OR EXISTS(SELECT 1 FROM activity_requests r WHERE r.identity=u.identity AND r.phase='active')", time.Now().UTC().Add(-30*time.Minute).Format("2006-01-02T15:04:05.000000000Z")).Scan(&o.Activity.RecentUsers); err != nil {
		return o, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_requests WHERE phase='active'").Scan(&o.Activity.ActiveRequests); err != nil {
		return o, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_requests WHERE phase='unknown'").Scan(&o.Activity.UnknownRequests); err != nil {
		return o, err
	}
	// The unmodified host has no authoritative in-flight snapshot. Continuity
	// proves delivery since observation began, not a complete initial baseline.
	o.Activity.BaselineComplete = false
	return o, tx.Commit()
}
func (s *Store) EventDisconnected(ctx context.Context, code string) error {
	switch code {
	case "bridge_unavailable", "bridge_stopping", "bridge_transport_failed", "bridge_response_invalid", "bridge_protocol_incompatible", "event_stream_failed":
	default:
		code = "event_stream_failed"
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE event_state SET connected=0,connection_error=? WHERE singleton=1 AND (connected!=0 OR connection_error!=?)", code, code)
		return err
	})
}
func validateBatch(b events.Batch) error {
	if !ValidID(b.Instance) || !ValidID(b.HostProcess) || len(b.Events) > events.BatchLimit || b.Latest >= math.MaxInt64 || b.LossEpoch > math.MaxInt64 || b.Earliest < 1 || b.Earliest > b.Latest+1 || b.After > b.Latest || b.Acknowledged > b.Latest {
		return errors.New("invalid_event_batch")
	}
	previous := b.After
	for _, e := range b.Events {
		if e.Sequence <= previous || e.Sequence > b.Latest || e.Sequence < b.Earliest || !ValidID(e.RequestID) || len(e.Identity) > 256 || len(e.AccountID) > 256 || e.At.IsZero() || e.At.After(time.Now().Add(5*time.Minute)) {
			return errors.New("invalid_event_batch")
		}
		switch e.Kind {
		case "started", "account_selected", "completed":
		default:
			return errors.New("invalid_event_kind")
		}
		if e.Kind == "started" && e.Identity == "" {
			return errors.New("invalid_event_identity")
		}
		previous = e.Sequence
	}
	return nil
}
func (s *Store) ApplyEvents(ctx context.Context, b events.Batch) (uint64, error) {
	if err := validateBatch(b); err != nil {
		return 0, err
	}
	var through uint64
	err := s.write(ctx, func(tx *sql.Tx) error {
		c, err := readCursor(tx.QueryRowContext(ctx, cursorSQL))
		if err != nil {
			return err
		}
		previous := c
		newInstance := c.Instance != b.Instance
		if newInstance {
			var retired int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM retired_bridges WHERE instance=?", b.Instance).Scan(&retired); err != nil {
				return err
			}
			if retired != 0 {
				return errors.New("retired_bridge_instance")
			}
			if b.After != 0 {
				return errors.New("invalid_event_cursor")
			}
			if c.Instance != "" {
				if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO retired_bridges VALUES(?)", c.Instance); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "DELETE FROM retired_bridges WHERE rowid NOT IN(SELECT rowid FROM retired_bridges ORDER BY rowid DESC LIMIT 128)"); err != nil {
					return err
				}
				phase := "unknown"
				if c.HostProcess != b.HostProcess {
					phase = "completed"
				}
				if _, err = tx.ExecContext(ctx, "UPDATE activity_requests SET phase=? WHERE phase!='completed'", phase); err != nil {
					return err
				}
			}
			status, reason := "observing", "initial_observation"
			if c.Instance != "" {
				status, reason = "gap", "bridge_restarted"
				if c.HostProcess != b.HostProcess {
					status, reason = "observing", "new_host_process"
				}
			}
			c = EventCursor{Instance: b.Instance, HostProcess: b.HostProcess, Status: status, Reason: reason}
		} else if c.HostProcess != b.HostProcess || b.After > c.Sequence || b.LossEpoch < c.LossEpoch {
			return errors.New("invalid_event_cursor")
		}
		// A replay may describe an earlier gap that this cursor already consumed.
		// Do not turn requests observed after that gap into unknown a second time.
		lost := (b.Gap && b.After >= c.Sequence) || c.Sequence < b.Acknowledged || c.Sequence+1 < b.Earliest || b.LossEpoch > c.LossEpoch
		if lost {
			c.Status, c.Reason = "gap", "events_lost"
			if _, err = tx.ExecContext(ctx, "UPDATE activity_requests SET phase='unknown' WHERE phase='active'"); err != nil {
				return err
			}
		}
		if c.Sequence+1 < b.Earliest {
			c.Sequence = b.Earliest - 1
		}
		for _, e := range b.Events {
			if e.Sequence <= c.Sequence {
				continue
			}
			if e.Sequence != c.Sequence+1 {
				return errors.New("noncontiguous_event_batch")
			}
			if err = applyActivity(tx, ctx, b.Instance, e, &c); err != nil {
				return err
			}
			c.Sequence = e.Sequence
		}
		if len(b.Events) == 0 && c.Sequence < b.Latest {
			return errors.New("incomplete_event_batch")
		}
		c.LossEpoch = b.LossEpoch
		through = c.Sequence
		lastSeen, _ := time.Parse(time.RFC3339Nano, previous.LastSeen)
		if c.Instance == previous.Instance && c.Sequence == previous.Sequence && c.LossEpoch == previous.LossEpoch && c.Status == previous.Status && c.Reason == previous.Reason && previous.Connected && previous.ConnectionError == "" && time.Since(lastSeen) < 5*time.Second {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE event_state SET bridge_instance=?,host_process=?,sequence=?,loss_epoch=?,status=?,reason=?,connected=1,connection_error='',last_seen=? WHERE singleton=1`, c.Instance, c.HostProcess, c.Sequence, c.LossEpoch, c.Status, c.Reason, now())
		return err
	})
	return through, err
}
func applyActivity(tx *sql.Tx, ctx context.Context, instance string, e events.Event, c *EventCursor) error {
	var identity, account, phase, started, last string
	err := tx.QueryRowContext(ctx, "SELECT identity,account_id,phase,started_at,last_event_at FROM activity_requests WHERE bridge_instance=? AND request_id=?", instance, e.RequestID).Scan(&identity, &account, &phase, &started, &last)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	stamp := e.At.UTC().Format("2006-01-02T15:04:05.000000000Z")
	if exists && (phase == "completed" || (stamp < last && e.Kind != "completed")) {
		return nil
	}
	// Completion is authoritative for its request even if the host wall clock
	// moved backwards. Keep timestamps monotonic for retention and aggregation.
	if exists && stamp < last {
		stamp = last
	}
	if !exists {
		// A selection without a caller scope cannot establish an eligible user
		// request. Still accept selections for a start we already observed.
		if e.Kind != "completed" && e.Identity == "" {
			return nil
		}
		var count int
		if e.Kind != "completed" {
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_requests WHERE phase!='completed'").Scan(&count); err != nil {
				return err
			}
			if count >= MaxTrackedRequests {
				c.Status, c.Reason = "gap", "activity_capacity_exceeded"
				return nil
			}
		}
		identity, account, phase, started = e.Identity, e.AccountID, "active", stamp
		if e.Kind == "completed" {
			identity = ""
		}
	}
	if e.Kind == "completed" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM activity_requests WHERE rowid IN(SELECT rowid FROM activity_requests WHERE phase='completed' ORDER BY last_event_at DESC,rowid DESC LIMIT -1 OFFSET 8191)"); err != nil {
			return err
		}
	}
	if e.Kind == "account_selected" {
		account = e.AccountID
	}
	if e.Kind == "completed" {
		phase = "completed"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO activity_requests VALUES(?,?,?,?,?,?,?) ON CONFLICT(bridge_instance,request_id) DO UPDATE SET account_id=excluded.account_id,phase=excluded.phase,last_event_at=excluded.last_event_at`, instance, e.RequestID, identity, account, phase, started, stamp); err != nil {
		return err
	}
	if identity != "" {
		var present, count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_users WHERE identity=?", identity).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM activity_users").Scan(&count); err != nil {
				return err
			}
			if count >= MaxRecentIdentities {
				c.Status, c.Reason = "gap", "identity_capacity_exceeded"
				return nil
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO activity_users VALUES(?,?) ON CONFLICT(identity) DO UPDATE SET last_activity=max(last_activity,excluded.last_activity)", identity, stamp); err != nil {
			return fmt.Errorf("activity aggregate: %w", err)
		}
	}
	return nil
}
