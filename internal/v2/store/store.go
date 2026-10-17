// Package store persists the independent read-only backend's jobs and evidence.
// Network work never runs inside a database transaction.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codex-window-refresh/internal/v2/policy"
	quotaevidence "codex-window-refresh/internal/v2/quota"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema string

var ErrConflict = errors.New("idempotency_conflict")
var ErrBusy = errors.New("queue_full")
var ErrNotFound = errors.New("not_found")
var ErrViewChanged = errors.New("view_changed")
var ErrJobExpired = errors.New("job_expired")

const MaxPendingItems = 4096

type Account struct {
	ID               string          `json:"id"`
	Provider         string          `json:"provider,omitempty"`
	AuthIndex        string          `json:"-"`
	Binding          string          `json:"-"`
	Status           string          `json:"status"`
	DisplayStatus    string          `json:"display_status,omitempty"`
	StatusObservedAt string          `json:"status_observed_at,omitempty"`
	Quota            json.RawMessage `json:"quota,omitempty"`
	ObservedAt       string          `json:"observed_at,omitempty"`
	PendingQuery     *PendingQuery   `json:"pending_query,omitempty"`
}
type Item struct {
	AccountID   string `json:"account_id"`
	Phase       string `json:"phase"`
	ErrorCode   string `json:"error_code,omitempty"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	Trigger     string `json:"trigger,omitempty"`
	HoldReason  string `json:"hold_reason,omitempty"`
}
type Job struct {
	InstanceID  string `json:"instance_id,omitempty"`
	Revision    uint64 `json:"view_revision"`
	ID          string `json:"job_id"`
	Target      string `json:"account_id,omitempty"`
	Phase       string `json:"phase"`
	ErrorCode   string `json:"error_code,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Items       []Item `json:"items"`
	Trigger     string `json:"trigger,omitempty"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	HoldReason  string `json:"hold_reason,omitempty"`
}
type Run struct {
	ID      string
	Account Account
}
type Store struct {
	db     *sql.DB
	lock   *os.File
	writer chan struct{}
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random identity unavailable")
	}
	return hex.EncodeToString(b[:])
}
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("data_dir_must_be_absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("data_dir_must_be_private_and_owned")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".daemon.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "daemon lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("backend_already_running")
	}
	path := filepath.Join(dir, "backend.sqlite")
	if old, e := os.Lstat(path); e == nil && (!old.Mode().IsRegular() || old.Mode().Perm() != 0600 || old.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid())) {
		lock.Close()
		return nil, errors.New("database_must_be_private_and_owned")
	} else if e != nil && !os.IsNotExist(e) {
		lock.Close()
		return nil, e
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	f.Close()
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db, lock: lock, writer: make(chan struct{}, 1)}
	// Reject foreign or newer databases before creating any application tables.
	var tables, versionTable int
	if err = db.QueryRow("SELECT count(*),COALESCE(sum(name='schema_version'),0) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables, &versionTable); err != nil {
		s.Close()
		return nil, err
	}
	if tables > 0 {
		var applicationID int
		if err = db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil || applicationID != 0x43575232 {
			s.Close()
			return nil, errors.New("unsupported_database_schema")
		}
		var count, minVersion, maxVersion int
		if versionTable != 1 {
			s.Close()
			return nil, errors.New("unsupported_database_schema")
		}
		if err = db.QueryRow("SELECT count(*),min(version),max(version) FROM schema_version").Scan(&count, &minVersion, &maxVersion); err != nil || count != 1 || minVersion < 1 || maxVersion > 8 {
			s.Close()
			return nil, errors.New("unsupported_database_schema")
		}
	}
	migration, err := db.Begin()
	if err != nil {
		s.Close()
		return nil, err
	}
	if _, err = migration.Exec(schema); err == nil {
		var providerColumn int
		err = migration.QueryRow("SELECT count(*) FROM pragma_table_info('prompt_generations') WHERE name='provider'").Scan(&providerColumn)
		if err == nil && providerColumn == 0 {
			_, err = migration.Exec("ALTER TABLE prompt_generations ADD COLUMN provider TEXT NOT NULL DEFAULT 'codex'")
		}
		if err == nil {
			var promptColumn int
			err = migration.QueryRow("SELECT count(*) FROM pragma_table_info('prompt_generations') WHERE name='prompt'").Scan(&promptColumn)
			if err == nil && promptColumn == 0 {
				_, err = migration.Exec("ALTER TABLE prompt_generations ADD COLUMN prompt TEXT NOT NULL DEFAULT ''")
			}
		}
		if err == nil {
			_, err = migration.Exec("UPDATE schema_version SET version=8")
		}
	}
	if err == nil {
		err = migration.Commit()
	} else {
		_ = migration.Rollback()
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 8 {
		s.Close()
		return nil, errors.New("unsupported_database_schema")
	}
	// Only this process owns the database. Interrupted read-only work can be
	// retried; the accepted job and its identity survive.
	_, err = db.Exec("UPDATE job_items SET phase='queued',run_id=NULL WHERE phase='running'; UPDATE query_runs SET phase='interrupted' WHERE phase='running'")
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { err := s.db.Close(); s.lock.Close(); return err }
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case s.writer <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writer }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Create(ctx context.Context, key, hash, target string) (string, bool, error) {
	return s.create(ctx, key, hash, target, nil)
}
func (s *Store) CreateQuery(ctx context.Context, key, hash, target string) (string, bool, error) {
	trigger := "manual_batch"
	if target != "" {
		trigger = "manual_single"
	}
	return s.create(ctx, key, hash, target, &QueryPlan{Trigger: trigger})
}
func (s *Store) create(ctx context.Context, key, hash, target string, plan *QueryPlan) (string, bool, error) {
	id, existing := ID(), false
	err := s.write(ctx, func(tx *sql.Tx) error {
		var receiptHash string
		err := tx.QueryRowContext(ctx, "SELECT job_id,request_hash FROM job_receipts WHERE idempotency_key=?", key).Scan(&id, &receiptHash)
		if err == nil {
			if receiptHash != hash {
				return ErrConflict
			}
			var present int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE id=?", id).Scan(&present); err != nil {
				return err
			}
			if present == 0 {
				return ErrJobExpired
			}
			existing = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previousHash string
		err = tx.QueryRowContext(ctx, "SELECT id,request_hash FROM jobs WHERE idempotency_key=?", key).Scan(&id, &previousHash)
		if err == nil {
			existing = true
			if hash != previousHash {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE phase IN ('queued','running','blocked')").Scan(&count); err != nil {
			return err
		}
		if count >= MaxPendingItems {
			return ErrBusy
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM job_receipts").Scan(&count); err != nil {
			return err
		}
		if count >= 50000 {
			return ErrBusy
		}
		stamp := now()
		if _, err = tx.ExecContext(ctx, "INSERT INTO job_receipts VALUES(?,?,?,?)", key, hash, id, time.Now().Add(30*24*time.Hour).Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES(?,?,?,?,'queued',?,?)", id, key, hash, target, stamp, stamp)
		if err != nil || plan == nil {
			return err
		}
		settings, err := readQuerySettings(ctx, tx)
		if err != nil {
			return err
		}
		copy := *plan
		if target == "" {
			copy.Min, copy.Max = settings.Config.Delay.Min, settings.Config.Delay.Max
		}
		return insertQueryPlan(ctx, tx, id, copy)
	})
	return id, existing, err
}
func (s *Store) Pending(ctx context.Context) (Job, error) {
	var j Job
	err := s.db.QueryRowContext(ctx, "SELECT id,target,created_at FROM jobs WHERE expanded=0 AND phase IN ('queued','blocked') ORDER BY created_at LIMIT 1").Scan(&j.ID, &j.Target, &j.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}
func (s *Store) Block(ctx context.Context, id, code string) error {
	// Dependency waits are bounded across restarts by the durable acceptance time.
	return s.write(ctx, func(tx *sql.Tx) error {
		var created string
		if err := tx.QueryRowContext(ctx, "SELECT created_at FROM jobs WHERE id=?", id).Scan(&created); err != nil {
			return err
		}
		at, _ := time.Parse(time.RFC3339Nano, created)
		phase, expanded := "blocked", 0
		if time.Since(at) > 2*time.Minute {
			phase, expanded = "failed", 1
		}
		_, err := tx.ExecContext(ctx, "UPDATE jobs SET phase=?,expanded=?,error_code=?,updated_at=? WHERE id=?", phase, expanded, code, now(), id)
		return err
	})
}
func (s *Store) Expand(ctx context.Context, j Job, accounts []Account) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var expanded int
		if err := tx.QueryRowContext(ctx, "SELECT expanded FROM jobs WHERE id=?", j.ID).Scan(&expanded); err != nil {
			return err
		}
		if expanded != 0 {
			return nil
		}
		plan, err := readQueryPlan(ctx, tx, j.ID, j.Target)
		if err != nil {
			return err
		}
		scheduled := time.Now().UTC()
		selected := []Account{}
		for _, a := range accounts {
			if j.Target == "" || j.Target == a.ID {
				selected = append(selected, a)
			}
		}
		code := ""
		if j.Target != "" && len(selected) == 0 {
			code = "account_not_found"
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM job_items WHERE phase IN ('queued','running')").Scan(&count); err != nil {
			return err
		}
		if len(selected)+count > MaxPendingItems {
			code = "queue_full"
			selected = nil
		}
		for _, a := range selected {
			if _, err := tx.ExecContext(ctx, "DELETE FROM account_bindings WHERE account_id=? AND EXISTS(SELECT 1 FROM accounts WHERE id=? AND auth_index!=?)", a.ID, a.ID, a.AuthIndex); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO accounts(id,auth_index,status,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET quota=CASE WHEN accounts.auth_index=excluded.auth_index THEN accounts.quota ELSE NULL END,observed_at=CASE WHEN accounts.auth_index=excluded.auth_index THEN accounts.observed_at ELSE NULL END,auth_index=excluded.auth_index,status=excluded.status,updated_at=excluded.updated_at", a.ID, a.AuthIndex, a.Status, now()); err != nil {
				return err
			}
			if err := insertQueryItem(ctx, tx, j.ID, a, plan, scheduled); err != nil {
				return err
			}
		}
		phase := "running"
		if len(selected) == 0 {
			phase = "completed"
		}
		if code != "" {
			phase = "failed"
		}
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET expanded=1,phase=?,error_code=?,updated_at=? WHERE id=?", phase, code, now(), j.ID)
		return err
	})
}
func (s *Store) Claim(ctx context.Context) (Run, error) {
	return s.ClaimAt(ctx, time.Now().UTC())
}
func (s *Store) Finish(ctx context.Context, r Run, quota json.RawMessage, code string) error {
	if code == "" && !json.Valid(quota) {
		return errors.New("invalid_quota_snapshot")
	}
	observedAt := ""
	if code == "" {
		var snapshot struct {
			ObservedAt time.Time `json:"observed_at"`
		}
		if json.Unmarshal(quota, &snapshot) != nil {
			return errors.New("invalid_quota_snapshot")
		}
		if !snapshot.ObservedAt.IsZero() {
			observedAt = snapshot.ObservedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	sum := sha256.Sum256(append(append([]byte(code), 0), quota...))
	hash := hex.EncodeToString(sum[:])
	return s.write(ctx, func(tx *sql.Tx) error {
		var previousPhase, previousHash string
		if err := tx.QueryRowContext(ctx, "SELECT phase,outcome_hash FROM query_runs WHERE id=?", r.ID).Scan(&previousPhase, &previousHash); err != nil {
			return err
		}
		if previousPhase == "finished" && previousHash == hash {
			return nil
		}
		res, err := tx.ExecContext(ctx, "UPDATE query_runs SET phase='finished',outcome_hash=? WHERE id=? AND phase='running'", hash, r.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("stale_query_result")
		}
		if code == "" {
			var binding, old string
			err = tx.QueryRowContext(ctx, "SELECT COALESCE(b.binding,''),COALESCE(a.quota,'') FROM accounts a LEFT JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=?", r.Account.ID, r.Account.AuthIndex).Scan(&binding, &old)
			if errors.Is(err, sql.ErrNoRows) || err == nil && r.Account.Binding != "" && binding != r.Account.Binding {
				code = "account_binding_changed"
			} else if err != nil {
				return err
			}
			if code == "" {
				var snapshot struct {
					Binding string `json:"binding"`
				}
				_ = json.Unmarshal(quota, &snapshot)
				if snapshot.Binding != "" {
					if binding != "" && binding != snapshot.Binding {
						code = "account_binding_changed"
					} else {
						merged, _, e := quotaevidence.Merge([]byte(old), quota)
						if e != nil {
							return e
						}
						quota = merged
						var stamp struct {
							ObservedAt time.Time `json:"observed_at"`
						}
						_ = json.Unmarshal(quota, &stamp)
						observedAt = stamp.ObservedAt.UTC().Format(time.RFC3339Nano)
					}
				}
			}
			if code == "" {
				result, updateErr := tx.ExecContext(ctx, "UPDATE accounts SET quota=?,observed_at=? WHERE id=? AND auth_index=?", string(quota), observedAt, r.Account.ID, r.Account.AuthIndex)
				if updateErr != nil {
					return updateErr
				}
				if n, _ := result.RowsAffected(); n != 1 {
					code = "account_binding_changed"
				}
			}
		}
		phase := "completed"
		if code != "" {
			phase = "failed"
		}
		if _, err = tx.ExecContext(ctx, "UPDATE job_items SET phase=?,error_code=? WHERE run_id=?", phase, code, r.ID); err != nil {
			return err
		}
		return finishQueryJobs(ctx, tx, time.Now().UTC())
	})
}

func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j := Job{Items: []Item{}}
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&j.Revision); err != nil {
		return j, err
	}
	err = tx.QueryRowContext(ctx, "SELECT id,target,phase,error_code,created_at,updated_at FROM jobs WHERE id=?", id).Scan(&j.ID, &j.Target, &j.Phase, &j.ErrorCode, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	plan, err := readQueryPlan(ctx, tx, id, j.Target)
	if err != nil {
		return j, err
	}
	j.Trigger = plan.Trigger
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return j, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT i.account_id,i.phase,i.error_code,COALESCE(q.scheduled_at,''),COALESCE(p.trigger,?) FROM job_items i LEFT JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id LEFT JOIN query_plans p ON p.job_id=i.job_id WHERE i.job_id=? ORDER BY i.account_id", plan.Trigger, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		var i Item
		if err = rows.Scan(&i.AccountID, &i.Phase, &i.ErrorCode, &i.ScheduledAt, &i.Trigger); err != nil {
			return j, err
		}
		if i.Phase == "queued" {
			if j.ScheduledAt == "" || i.ScheduledAt < j.ScheduledAt {
				j.ScheduledAt = i.ScheduledAt
			}
			if plan.Automatic && !settings.Config.Allowed(time.Now()) {
				i.HoldReason = "excluded_hours"
			}
		}
		j.Items = append(j.Items, i)
	}
	if err = rows.Err(); err != nil {
		return j, err
	}
	rows.Close()
	for _, item := range j.Items {
		if item.Phase == "running" {
			j.HoldReason = ""
			break
		}
		if item.HoldReason != "" {
			j.HoldReason = item.HoldReason
		}
	}
	return j, tx.Commit()
}
func (s *Store) Accounts(ctx context.Context, after string, limit int) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,auth_index,status,COALESCE(quota,''),COALESCE(observed_at,'') FROM accounts WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Account{}
	for rows.Next() {
		var a Account
		var q string
		if err = rows.Scan(&a.ID, &a.AuthIndex, &a.Status, &q, &a.ObservedAt); err != nil {
			return nil, err
		}
		if q != "" {
			a.Quota = json.RawMessage(q)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// Public errors are fixed classifications. SQL and filesystem error strings
// belong to local diagnosis, never API payloads that could expose paths/data.
func PublicError(err error) string {
	for _, known := range []error{ErrConflict, ErrBusy, ErrNotFound, ErrViewChanged, ErrJobExpired, ErrSettingsConflict, policy.ErrInvalid} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "storage_unavailable"
}
func ValidID(id string) bool {
	return len(id) > 0 && len(id) <= 256 && !strings.ContainsAny(id, "\x00\r\n")
}
func (s *Store) Check(ctx context.Context) error { return s.db.PingContext(ctx) }
