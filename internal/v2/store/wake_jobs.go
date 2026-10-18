package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/policy"
)

type WakeRequest struct {
	AccountID string `json:"account_id"`
}

type WakeJob struct {
	Revision             uint64        `json:"view_revision"`
	ID                   string        `json:"job_id"`
	AccountID            string        `json:"account_id"`
	Reason               string        `json:"reason"`
	Phase                string        `json:"phase"`
	ErrorCode            string        `json:"error_code,omitempty"`
	HoldReason           string        `json:"hold_reason,omitempty"`
	ScheduledAt          string        `json:"scheduled_at"`
	CreatedAt            string        `json:"created_at"`
	UpdatedAt            string        `json:"updated_at"`
	InstanceID           string        `json:"instance_id,omitempty"`
	Response             *WakeResponse `json:"response,omitempty"`
	Account              Account       `json:"-"`
	Cycle                string        `json:"-"`
	QueryJobID           string        `json:"-"`
	QueryGate            string        `json:"-"`
	Model                string        `json:"-"`
	Prompt               string        `json:"-"`
	PolicyRevision       uint64        `json:"-"`
	ModelRevision        uint64        `json:"-"`
	VerificationAttempts int           `json:"-"`
	HoldUntil            string        `json:"-"`
	NextCheckAt          string        `json:"-"`
}

type WakeJobPage struct {
	Jobs       []WakeJob `json:"jobs"`
	Revision   uint64    `json:"view_revision"`
	InstanceID string    `json:"instance_id,omitempty"`
}

const wakeColumns = "id,account_id,auth_index,binding,reason,cycle,phase,error_code,hold_reason,scheduled_at,created_at,updated_at,query_job_id,query_gate,model,prompt,policy_revision,model_revision,verification_attempts,hold_until,next_check_at"

func scanWake(row interface{ Scan(...any) error }) (WakeJob, error) {
	var job WakeJob
	err := row.Scan(&job.ID, &job.AccountID, &job.Account.AuthIndex, &job.Account.Binding, &job.Reason, &job.Cycle, &job.Phase, &job.ErrorCode, &job.HoldReason, &job.ScheduledAt, &job.CreatedAt, &job.UpdatedAt, &job.QueryJobID, &job.QueryGate, &job.Model, &job.Prompt, &job.PolicyRevision, &job.ModelRevision, &job.VerificationAttempts, &job.HoldUntil, &job.NextCheckAt)
	job.Account.ID, job.Account.Status, job.Account.Provider = job.AccountID, "available", "codex"
	return job, err
}

func (st *Store) Wake(ctx context.Context, id string) (WakeJob, error) {
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WakeJob{}, err
	}
	defer tx.Rollback()
	var revision uint64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&revision); err != nil {
		return WakeJob{}, err
	}
	job, err := scanWake(tx.QueryRowContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE id=?", id))
	job.Revision = revision
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		job.Response, err = loadWakeResponse(ctx, tx, id)
	}
	if err == nil {
		err = tx.Commit()
	}
	return job, err
}

func (st *Store) WakeJobs(ctx context.Context, limit int) (WakeJobPage, error) {
	page := WakeJobPage{Jobs: []WakeJob{}}
	if limit < 1 || limit > 100 {
		return page, ErrInvalidWakeRequest
	}
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&page.Revision); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs ORDER BY updated_at DESC,id DESC LIMIT ?", limit)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		job, scanErr := scanWake(rows)
		if scanErr != nil {
			rows.Close()
			return page, scanErr
		}
		page.Jobs = append(page.Jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	for index := range page.Jobs {
		page.Jobs[index].Revision = page.Revision
		page.Jobs[index].Response, err = loadWakeResponse(ctx, tx, page.Jobs[index].ID)
		if err != nil {
			return page, err
		}
	}
	return page, tx.Commit()
}

func wakeRequestHash(key string, request WakeRequest) (string, error) {
	if !ValidID(request.AccountID) || len(key) < 1 || len(key) > 128 || strings.ContainsFunc(key, func(value rune) bool { return value < 33 || value > 126 }) {
		return "", ErrInvalidWakeRequest
	}
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (st *Store) LookupWake(ctx context.Context, key string, request WakeRequest) (WakeJob, bool, error) {
	hash, err := wakeRequestHash(key, request)
	if err != nil {
		return WakeJob{}, false, err
	}
	var id, previous string
	err = st.db.QueryRowContext(ctx, "SELECT id,request_hash FROM wake_jobs WHERE idempotency_key=?", key).Scan(&id, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return WakeJob{}, false, nil
	}
	if err != nil {
		return WakeJob{}, false, err
	}
	if hash != previous {
		return WakeJob{}, true, ErrConflict
	}
	job, err := st.Wake(ctx, id)
	return job, true, err
}

func (st *Store) CreateWake(ctx context.Context, key string, request WakeRequest) (WakeJob, bool, error) {
	hash, err := wakeRequestHash(key, request)
	if err != nil {
		return WakeJob{}, false, err
	}
	var job WakeJob
	existing := false
	err = st.write(ctx, func(tx *sql.Tx) error {
		var id, previous string
		err := tx.QueryRowContext(ctx, "SELECT id,request_hash FROM wake_jobs WHERE idempotency_key=?", key).Scan(&id, &previous)
		if err == nil {
			if previous != hash {
				return ErrConflict
			}
			existing = true
			job, err = scanWake(tx.QueryRowContext(ctx, "SELECT "+wakeColumns+" FROM wake_jobs WHERE id=?", id))
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		account, err := generationAccount(ctx, tx, request.AccountID)
		if errors.Is(err, ErrGenerationAccount) {
			return ErrWakeAccount
		}
		if err != nil {
			return err
		}
		model, err := readModelSettings(ctx, tx)
		if err != nil {
			return err
		}
		if model.Config.DefaultModel == "" {
			return ErrWakeModelMissing
		}
		var pending, total int
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM wake_jobs WHERE account_id=? AND binding=? AND phase IN ('queued','checking','prepared','sending','verifying','unknown')),(SELECT count(*) FROM wake_jobs)", account.ID, account.Binding).Scan(&pending, &total); err != nil {
			return err
		}
		if pending > 0 || total >= 10000 {
			return ErrBusy
		}
		stamp := now()
		job = WakeJob{ID: ID(), AccountID: account.ID, Account: account, Reason: "manual", Cycle: key, Phase: "queued", ScheduledAt: stamp, CreatedAt: stamp, UpdatedAt: stamp}
		_, err = tx.ExecContext(ctx, "INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,idempotency_key,request_hash,scheduled_at,created_at,updated_at) VALUES(?,?,?,?,'manual',?,'queued',?,?,?,?,?)", job.ID, account.ID, account.AuthIndex, account.Binding, key, key, hash, stamp, stamp, stamp)
		return err
	})
	return job, existing, err
}

func updateWake(ctx context.Context, tx *sql.Tx, job WakeJob, phase, code, hold string, at time.Time) error {
	next := ""
	if phase == "queued" || phase == "checking" || phase == "verifying" || phase == "unknown" {
		next = queryStamp(at.Add(time.Second))
	}
	_, err := tx.ExecContext(ctx, "UPDATE wake_jobs SET phase=?,error_code=?,hold_reason=?,updated_at=?,next_check_at=? WHERE id=? AND (phase!=? OR error_code!=? OR hold_reason!=? OR next_check_at!=?)", phase, code, hold, queryStamp(at), next, job.ID, phase, code, hold, next)
	return err
}

func (st *Store) RecoverWakes(ctx context.Context) error {
	return st.write(ctx, func(tx *sql.Tx) error {
		interrupted := NormalizeWakeResponse(WakeResponse{RecordedAt: now(), Stage: "interrupted", ErrorCode: "wake_interrupted"})
		raw, err := json.Marshal(interrupted)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO wake_responses(job_id,detail) SELECT id,? FROM wake_jobs WHERE phase='sending' ON CONFLICT(job_id) DO UPDATE SET detail=excluded.detail", string(raw)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE wake_jobs SET phase='queued',model='',prompt='',hold_reason='',updated_at=? WHERE phase='prepared'", now()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET phase='unknown',error_code='wake_interrupted',hold_reason='verification_pending',query_job_id='',query_gate=?,updated_at=? WHERE phase='sending'", now(), now())
		return err
	})
}

func (st *Store) FinishWake(ctx context.Context, id, phase, code string, at time.Time) error {
	return st.FinishWakeWithResponse(ctx, id, phase, code, at, nil)
}

func (st *Store) FinishWakeWithResponse(ctx context.Context, id, phase, code string, at time.Time, response *WakeResponse) error {
	if phase != "verifying" && phase != "unknown" && phase != "failed" && !(phase == "queued" && code == "model_busy") {
		return ErrInvalidWakeRequest
	}
	switch code {
	case "", "wake_outcome_unknown", "wake_interrupted", "wake_request_rejected", "wake_account_unavailable", "model_busy":
	default:
		code = "wake_outcome_unknown"
	}
	return st.write(ctx, func(tx *sql.Tx) error {
		var result sql.Result
		var err error
		if phase == "queued" {
			result, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET phase='queued',error_code='',hold_reason='model_busy',next_check_at=?,query_gate='',model='',prompt='',hold_until='',updated_at=? WHERE id=? AND phase='sending'", queryStamp(at.Add(time.Second)), queryStamp(at), id)
		} else {
			result, err = tx.ExecContext(ctx, "UPDATE wake_jobs SET phase=?,error_code=?,hold_reason=?,query_job_id='',query_gate=?,scheduled_at=?,updated_at=? WHERE id=? AND phase='sending'", phase, code, "verification_pending", queryStamp(at), queryStamp(at), queryStamp(at), id)
		}
		if err != nil || response == nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		copy := NormalizeWakeResponse(*response)
		copy.RecordedAt = queryStamp(at)
		raw, err := json.Marshal(copy)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO wake_responses(job_id,detail) VALUES(?,?) ON CONFLICT(job_id) DO UPDATE SET detail=excluded.detail", id, string(raw))
		return err
	})
}

func wakeSettingsPermit(config policy.WakeConfig, job WakeJob, at time.Time) bool {
	if !config.RuleEnabled(job.Reason) {
		return false
	}
	if job.Reason == "daily" {
		date, _, _, ready := config.DailyBounds(at)
		return ready && job.Cycle == date
	}
	return true
}
