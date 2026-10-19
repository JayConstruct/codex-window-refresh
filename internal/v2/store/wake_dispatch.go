package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

func wakeAdmission(ctx context.Context, tx *sql.Tx, job WakeJob, config policy.WakeConfig, at time.Time) (string, error) {
	ready, err := wakeInventoryReady(ctx, tx, at)
	if err != nil {
		return "", err
	}
	if !ready {
		return "inventory_unavailable", nil
	}
	if !wakeSettingsPermit(config, job, at) {
		return "wake_rule_disabled", nil
	}
	hold, err := wakeActivityHold(ctx, tx, job.AccountID, at)
	if err != nil || hold != "" {
		return hold, err
	}
	if job.Reason != "manual" && job.Reason != "weekly_reset" {
		missing, users, err := wakeCapacity(ctx, tx, config, at, job.ID)
		if err != nil {
			return "", err
		}
		if !config.InAllowedHours(at) && !(job.Reason == "five_hour_reset" && users > 0) {
			return "outside_allowed_hours", nil
		}
		if missing <= 0 {
			return "capacity_satisfied", nil
		}
	}
	return wakeModelBusy(ctx, tx, job)
}

func holdUnsentWake(ctx context.Context, tx *sql.Tx, job WakeJob, hold string, at time.Time) error {
	phase := "queued"
	if hold == "capacity_satisfied" {
		phase = "skipped"
	}
	return updateWake(ctx, tx, job, phase, "", hold, at)
}

func (st *Store) PrepareWake(ctx context.Context, at time.Time) (WakeJob, error) {
	var prepared WakeJob
	found := false
	err := st.write(ctx, func(tx *sql.Tx) error {
		settings, err := readWakeSettings(ctx, tx)
		if err != nil {
			return err
		}
		models, err := readModelSettings(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE phase IN ('queued','checking') AND scheduled_at<=? AND next_check_at<=? ORDER BY updated_at,CASE reason WHEN 'weekly_reset' THEN 0 WHEN 'manual' THEN 1 ELSE 2 END,scheduled_at,id LIMIT 32", queryStamp(at), queryStamp(at))
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
			account, err := wakeBoundAccount(ctx, tx, job, at)
			if err != nil {
				if err = wakePreflightFailure(ctx, tx, job, err, at); err != nil {
					return err
				}
				continue
			}
			hold, err := wakeAdmission(ctx, tx, job, settings.Config, at)
			if err != nil {
				return err
			}
			if hold == "wake_rule_disabled" {
				if err = updateWake(ctx, tx, job, "cancelled", hold, "", at); err != nil {
					return err
				}
				continue
			}
			if hold != "" {
				if err = holdUnsentWake(ctx, tx, job, hold, at); err != nil {
					return err
				}
				continue
			}
			baseline, err := readWakeBaseline(ctx, tx, account)
			if err != nil {
				return err
			}
			var snapshot quota.Snapshot
			_ = json.Unmarshal(account.Quota, &snapshot)
			if job.QueryGate == "" {
				job.QueryGate = queryStamp(at)
				if wakeFresh(snapshot, account.Binding, "", at) {
					job.QueryGate = wakeSnapshotGate(snapshot)
				}
				if _, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET query_gate=? WHERE id=?", job.QueryGate, job.ID); err != nil {
					return err
				}
			}
			decision := wakeQuotaDecision(snapshot, baseline, job, at)
			if decision == "active_window" {
				if err = updateWake(ctx, tx, job, "skipped", "", "window_already_active", at); err != nil {
					return err
				}
				continue
			}
			if decision != "" {
				if job.QueryJobID != "" {
					var phase string
					err := tx.QueryRowContext(ctx, "SELECT phase FROM job_items WHERE job_id=? AND account_id=?", job.QueryJobID, job.AccountID).Scan(&phase)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					if phase != "queued" && phase != "running" {
						job.QueryJobID = ""
						job.QueryGate = queryStamp(at)
						if _, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET query_job_id='',query_gate=?,next_check_at=?,hold_reason=?,updated_at=? WHERE id=?", job.QueryGate, queryStamp(at.Add(30*time.Second)), decision, queryStamp(at), job.ID); err != nil {
							return err
						}
						continue
					}
				}
				if _, err = enqueueWakeRead(ctx, tx, &job, at); err != nil {
					if errors.Is(err, ErrBusy) {
						continue
					}
					return err
				}
				if err = updateWake(ctx, tx, job, "checking", "", decision, at); err != nil {
					return err
				}
				continue
			}
			model, prompt := policy.WakeModel(models.Config, job.Reason)
			if model == "" || strings.TrimSpace(prompt) == "" {
				if err = updateWake(ctx, tx, job, job.Phase, "", "model_missing", at); err != nil {
					return err
				}
				continue
			}
			if models.Config.PromptMode == "random" {
				if len(models.Config.Phrases) == 0 {
					return policy.ErrInvalidModelSettings
				}
				choice, err := rand.Int(rand.Reader, big.NewInt(int64(len(models.Config.Phrases))))
				if err != nil {
					return err
				}
				prompt = models.Config.Phrases[choice.Int64()]
			}
			_, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET phase='prepared',hold_reason='',error_code='',model=?,prompt=?,policy_revision=?,model_revision=?,hold_until=?,updated_at=? WHERE id=?", model, prompt, settings.Revision, models.Revision, queryStamp(at.Add(10*time.Minute)), queryStamp(at), job.ID)
			if err != nil {
				return err
			}
			job.Phase, job.HoldReason, job.ErrorCode, job.Model, job.Prompt, job.PolicyRevision, job.ModelRevision = "prepared", "", "", model, prompt, settings.Revision, models.Revision
			prepared = job
			found = true
			return nil
		}
		return nil
	})
	if err != nil {
		return WakeJob{}, err
	}
	if !found {
		return WakeJob{}, ErrNotFound
	}
	return prepared, nil
}

func (st *Store) AuthorizeWake(ctx context.Context, id string, at time.Time) (WakeJob, error) {
	var authorized WakeJob
	found := false
	err := st.write(ctx, func(tx *sql.Tx) error {
		job, err := scanWake(tx.QueryRowContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE id=? AND phase='prepared'", id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		settings, err := readWakeSettings(ctx, tx)
		if err != nil {
			return err
		}
		models, err := readModelSettings(ctx, tx)
		if err != nil {
			return err
		}
		account, err := wakeBoundAccount(ctx, tx, job, at)
		if err != nil {
			return wakePreflightFailure(ctx, tx, job, err, at)
		}
		hold, err := wakeAdmission(ctx, tx, job, settings.Config, at)
		if err != nil {
			return err
		}
		if hold == "wake_rule_disabled" {
			return updateWake(ctx, tx, job, "cancelled", hold, "", at)
		}
		if settings.Revision != job.PolicyRevision || models.Revision != job.ModelRevision {
			return updateWake(ctx, tx, job, "queued", "", "configuration_changed", at)
		}
		if hold != "" {
			return holdUnsentWake(ctx, tx, job, hold, at)
		}
		baseline, err := readWakeBaseline(ctx, tx, account)
		if err != nil {
			return err
		}
		var snapshot quota.Snapshot
		_ = json.Unmarshal(account.Quota, &snapshot)
		decision := wakeQuotaDecision(snapshot, baseline, job, at)
		if decision == "active_window" {
			return updateWake(ctx, tx, job, "skipped", "", "window_already_active", at)
		}
		if decision != "" {
			return updateWake(ctx, tx, job, "queued", "", decision, at)
		}
		if err = updateWake(ctx, tx, job, "sending", "", "", at); err != nil {
			return err
		}
		job.Phase = "sending"
		authorized = job
		found = true
		return nil
	})
	if err != nil {
		return WakeJob{}, err
	}
	if !found {
		return WakeJob{}, ErrNotFound
	}
	return authorized, nil
}
