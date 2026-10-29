package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

type SchedulingSettings struct {
	Revision uint64                  `json:"revision"`
	Config   policy.SchedulingConfig `json:"config"`
}

func readScheduling(ctx context.Context, tx *sql.Tx) (SchedulingSettings, error) {
	s := SchedulingSettings{Config: policy.DefaultScheduling()}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT revision,config FROM scheduling_settings WHERE singleton=1").Scan(&s.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if json.Unmarshal([]byte(raw), &s.Config) != nil || s.Config.Validate() != nil {
		return s, policy.ErrScheduling
	}
	return s, nil
}
func (s *Store) SchedulingSettings(ctx context.Context) (SchedulingSettings, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SchedulingSettings{}, err
	}
	defer tx.Rollback()
	return readScheduling(ctx, tx)
}
func (s *Store) SaveScheduling(ctx context.Context, expected uint64, c policy.SchedulingConfig) (SchedulingSettings, error) {
	if c.Projects == nil {
		c.Projects = []policy.ProjectPreference{}
	}
	result := SchedulingSettings{}
	if err := c.Validate(); err != nil {
		return result, err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		old, err := readScheduling(ctx, tx)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return ErrSettingsConflict
		}
		raw, _ := json.Marshal(c)
		previous, _ := json.Marshal(old.Config)
		result = SchedulingSettings{old.Revision, c}
		if string(raw) == string(previous) {
			return nil
		}
		result.Revision++
		_, err = tx.ExecContext(ctx, "INSERT INTO scheduling_settings VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET revision=excluded.revision,config=excluded.config", result.Revision, string(raw))
		return err
	})
	return result, err
}

type RoutingAccount struct {
	ID         string `json:"id"`
	Index      string `json:"-"`
	Binding    string `json:"-"`
	ValidUntil int64  `json:"valid_until"`
	ResetAt    int64  `json:"reset_at"`
	CardExpiry int64  `json:"card_expiry"`
}
type RoutingSnapshot struct {
	Config   policy.SchedulingConfig `json:"config"`
	Accounts []RoutingAccount        `json:"accounts"`
	Blocked  []string                `json:"blocked"`
}

func (s *Store) RoutingSnapshot(ctx context.Context, at time.Time) (RoutingSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RoutingSnapshot{}, err
	}
	defer tx.Rollback()
	settings, err := readScheduling(ctx, tx)
	if err != nil {
		return RoutingSnapshot{}, err
	}
	result := RoutingSnapshot{Config: settings.Config, Accounts: []RoutingAccount{}}
	holds, err := tx.QueryContext(ctx, "SELECT DISTINCT account_id FROM reset_attempts WHERE phase IN ('sending','unknown','verifying') AND EXISTS(SELECT 1 FROM account_bindings b WHERE b.account_id=reset_attempts.account_id AND b.binding=reset_attempts.binding) ORDER BY account_id LIMIT 10000")
	if err != nil {
		return result, err
	}
	for holds.Next() {
		var id string
		if err = holds.Scan(&id); err != nil {
			holds.Close()
			return result, err
		}
		result.Blocked = append(result.Blocked, id)
	}
	err = holds.Err()
	holds.Close()
	if err != nil {
		return result, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.auth_index,b.binding,a.quota,COALESCE(c.credits,''),COALESCE(c.observed_at,'') FROM accounts a JOIN account_bindings b ON b.account_id=a.id LEFT JOIN reset_cards c ON c.account_id=a.id AND c.binding=b.binding WHERE a.status='available' AND a.quota IS NOT NULL AND NOT EXISTS(SELECT 1 FROM reset_attempts r WHERE r.account_id=a.id AND r.binding=b.binding AND r.phase IN ('sending','unknown','verifying')) AND (SELECT last_success_at FROM inventory_state WHERE singleton=1)>=? ORDER BY a.id LIMIT 10000`, queryStamp(at.Add(-time.Minute)))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var a RoutingAccount
		var raw, cards, observed string
		if err = rows.Scan(&a.ID, &a.Index, &a.Binding, &raw, &cards, &observed); err != nil {
			return result, err
		}
		var q quota.Snapshot
		if json.Unmarshal([]byte(raw), &q) != nil || q.Binding != a.Binding {
			continue
		}
		valid, reset := routingEvidence(q, settings.Config.QuotaMaxAgeSeconds, at)
		if valid.IsZero() {
			continue
		}
		a.ValidUntil = valid.Unix()
		a.ResetAt = reset.Unix()
		if reset.IsZero() {
			a.ResetAt = 0
		}
		var credits policy.ResetCredits
		stamp, _ := time.Parse(time.RFC3339Nano, observed)
		if settings.Config.Cards.Enabled && stamp.Add(time.Duration(settings.Config.Cards.PollSeconds)*time.Second).After(at) && json.Unmarshal([]byte(cards), &credits) == nil && (q.ResetCredits == nil || q.Fields["reset_credits"].ObservedAt.Before(stamp) || q.ResetCredits.AvailableCount > 0 && q.ResetCredits.AvailableCount == credits.AvailableCount) {
			for _, c := range credits.Credits {
				if c.Status == "available" && c.ResetType == "codexRateLimits" && c.ExpiresAt != nil && *c.ExpiresAt > at.Unix() && *c.ExpiresAt <= at.Unix()+int64(settings.Config.Cards.ExpirySeconds) && (a.CardExpiry == 0 || *c.ExpiresAt < a.CardExpiry) {
					a.CardExpiry = *c.ExpiresAt
				}
			}
		}
		result.Accounts = append(result.Accounts, a)
	}
	return result, rows.Err()
}
func routingEvidence(q quota.Snapshot, maxAge int, at time.Time) (time.Time, time.Time) {
	if q.Permission == nil || *q.Permission != core.PermissionAllowed {
		return time.Time{}, time.Time{}
	}
	valid := at.Add(time.Duration(maxAge) * time.Second)
	var reset time.Time
	for name, w := range map[string]*core.WindowEvidence{"five_hour": q.FiveHour, "weekly": q.Weekly} {
		stamp := q.Fields[name].ObservedAt
		if stamp.IsZero() {
			stamp = q.ObservedAt
		}
		until := stamp.Add(time.Duration(maxAge) * time.Second)
		if w == nil || stamp.After(at.Add(time.Second)) || !until.After(at) || w.Remaining == nil || w.Remaining.Unit != "percent" || w.Remaining.Value <= 0 || w.Remaining.Value > 100 {
			return time.Time{}, time.Time{}
		}
		if w.State != core.WindowActive && w.State != core.WindowInactive {
			return time.Time{}, time.Time{}
		}
		if until.Before(valid) {
			valid = until
		}
		if w.State == core.WindowActive {
			if !w.ResetAt.After(at) {
				return time.Time{}, time.Time{}
			}
			if w.ResetAt.Before(valid) {
				valid = w.ResetAt
			}
			if reset.IsZero() || w.ResetAt.Before(reset) {
				reset = w.ResetAt
			}
		}
	}
	stamp := q.Fields["permission"].ObservedAt
	if stamp.IsZero() {
		stamp = q.ObservedAt
	}
	if stamp.After(at.Add(time.Second)) || !stamp.Add(time.Duration(maxAge)*time.Second).After(at) {
		return time.Time{}, time.Time{}
	}
	if stamp.Add(time.Duration(maxAge) * time.Second).Before(valid) {
		valid = stamp.Add(time.Duration(maxAge) * time.Second)
	}
	return valid, reset
}

type ResetView struct {
	AccountID         string               `json:"account_id"`
	Credits           *policy.ResetCredits `json:"credits"`
	ObservedAt        string               `json:"observed_at"`
	ErrorCode         string               `json:"error_code"`
	SummaryObservedAt string               `json:"summary_observed_at,omitempty"`
	DetailsObservedAt string               `json:"details_observed_at,omitempty"`
}
type ResetAttempt struct {
	ID        string  `json:"id"`
	AccountID string  `json:"account_id"`
	CreditID  string  `json:"credit_id"`
	Phase     string  `json:"phase"`
	Outcome   string  `json:"outcome"`
	ErrorCode string  `json:"error_code"`
	UpdatedAt string  `json:"updated_at"`
	Account   Account `json:"-"`
}

func (s *Store) ResetViews(ctx context.Context) ([]ResetView, []ResetAttempt, error) {
	views := []ResetView{}
	attempts := []ResetAttempt{}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,c.credits,COALESCE(c.observed_at,''),COALESCE(c.error_code,''),COALESCE(a.quota,''),b.binding FROM accounts a JOIN account_bindings b ON b.account_id=a.id LEFT JOIN reset_cards c ON c.account_id=a.id AND c.binding=b.binding WHERE a.status!='removed' ORDER BY a.id LIMIT 10000`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var v ResetView
		var raw sql.NullString
		var quotaRaw, binding string
		if err = rows.Scan(&v.AccountID, &raw, &v.ObservedAt, &v.ErrorCode, &quotaRaw, &binding); err != nil {
			break
		}
		if raw.Valid {
			_ = json.Unmarshal([]byte(raw.String), &v.Credits)
		}
		if v.Credits != nil && v.Credits.Credits != nil {
			v.DetailsObservedAt = v.ObservedAt
		}
		var snapshot quota.Snapshot
		if json.Unmarshal([]byte(quotaRaw), &snapshot) == nil && snapshot.Binding == binding && snapshot.ResetCredits != nil {
			summaryAt := snapshot.Fields["reset_credits"].ObservedAt
			if summaryAt.IsZero() {
				summaryAt = snapshot.ObservedAt
			}
			if !summaryAt.IsZero() {
				v.SummaryObservedAt = queryStamp(summaryAt)
				detailAt, _ := time.Parse(time.RFC3339Nano, v.ObservedAt)
				if v.Credits == nil || !summaryAt.Before(detailAt) {
					count := snapshot.ResetCredits.AvailableCount
					if v.Credits == nil || v.Credits.AvailableCount != count {
						v.Credits = &policy.ResetCredits{AvailableCount: count}
						v.DetailsObservedAt = ""
					}
					if count == 0 {
						v.Credits.Credits = []policy.ResetCredit{}
						v.DetailsObservedAt = ""
					}
					v.ObservedAt = queryStamp(summaryAt)
				}
			}
		}
		if v.Credits != nil || raw.Valid || v.ErrorCode != "" {
			views = append(views, v)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT id,account_id,credit_id,phase,outcome,error_code,updated_at FROM reset_attempts ORDER BY updated_at DESC LIMIT 100")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a ResetAttempt
		if err = rows.Scan(&a.ID, &a.AccountID, &a.CreditID, &a.Phase, &a.Outcome, &a.ErrorCode, &a.UpdatedAt); err != nil {
			return nil, nil, err
		}
		attempts = append(attempts, a)
	}
	return views, attempts, rows.Err()
}
func (s *Store) NextResetAccount(ctx context.Context, at time.Time) (Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.auth_index,a.status,b.binding FROM accounts a JOIN account_bindings b ON b.account_id=a.id LEFT JOIN reset_cards c ON c.account_id=a.id AND c.binding=b.binding WHERE a.status IN ('available','unavailable') AND COALESCE(c.next_at,'')<=? AND (SELECT last_success_at FROM inventory_state WHERE singleton=1)>=? ORDER BY COALESCE(c.next_at,''),a.id LIMIT 1`, queryStamp(at), queryStamp(at.Add(-time.Minute))).Scan(&a.ID, &a.AuthIndex, &a.Status, &a.Binding)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}
func (s *Store) RecordResetCredits(ctx context.Context, a Account, credits *policy.ResetCredits, code string, at time.Time, poll int) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var raw any
		if credits != nil {
			b, err := json.Marshal(credits)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		next := at.Add(time.Duration(poll) * time.Second)
		if code != "" && poll > 60 {
			next = at.Add(time.Minute)
		}
		if credits != nil {
			for _, card := range credits.Credits {
				if card.Status == "available" && card.ExpiresAt != nil && *card.ExpiresAt > at.Unix() {
					probe := time.Unix(*card.ExpiresAt, 0).Add(-time.Minute)
					if probe.Before(at.Add(time.Minute)) {
						probe = at.Add(time.Minute)
					}
					if probe.Before(next) {
						next = probe
					}
				}
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO reset_cards SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=? AND b.binding=?) ON CONFLICT(account_id) DO UPDATE SET binding=excluded.binding,credits=CASE WHEN excluded.error_code='' THEN excluded.credits WHEN reset_cards.binding=excluded.binding THEN reset_cards.credits ELSE NULL END,observed_at=CASE WHEN excluded.error_code='' THEN excluded.observed_at WHEN reset_cards.binding=excluded.binding THEN reset_cards.observed_at ELSE '' END,next_at=excluded.next_at,error_code=excluded.error_code`, a.ID, a.Binding, raw, queryStamp(at), queryStamp(next), code, a.ID, a.AuthIndex, a.Binding)
		return err
	})
}

// Persist ownership and the redemption key before any external mutation.
func (s *Store) ClaimReset(ctx context.Context, a Account, c policy.ResetCredit, at time.Time) (ResetAttempt, error) {
	job := ResetAttempt{}
	err := s.write(ctx, func(tx *sql.Tx) error {
		settings, err := readScheduling(ctx, tx)
		if err != nil {
			return err
		}
		if !settings.Config.Cards.Enabled || !settings.Config.Cards.AutoConsume {
			return ErrNotFound
		}
		var raw string
		var count int
		err = tx.QueryRowContext(ctx, `SELECT a.quota FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=? AND b.binding=? AND a.status IN ('available','unavailable') AND (SELECT last_success_at FROM inventory_state WHERE singleton=1)>=?`, a.ID, a.AuthIndex, a.Binding, queryStamp(at.Add(-time.Minute))).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var q quota.Snapshot
		if json.Unmarshal([]byte(raw), &q) != nil || q.Binding != a.Binding || !resetWorthwhile(q, c, settings.Config.Cards, at) {
			return ErrNotFound
		}
		// Background sends and reset redemption share account ownership. Foreground
		// routing learns this hold through the next expiring native snapshot.
		err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM wake_jobs WHERE account_id=? AND phase IN ('prepared','sending','verifying','unknown'))+(SELECT count(*) FROM prompt_generations WHERE account_id=? AND phase IN ('running','unknown'))+(SELECT count(*) FROM reset_attempts WHERE account_id=? AND binding=? AND (phase IN ('sending','unknown','verifying') OR (credit_id=? AND phase='completed') OR (phase='skipped' AND updated_at>?)))`, a.ID, a.ID, a.ID, a.Binding, c.ID, queryStamp(at.Add(-15*time.Minute))).Scan(&count)
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrNotFound
		}
		job = ResetAttempt{ID: ID(), AccountID: a.ID, CreditID: c.ID, Phase: "sending", Account: a, UpdatedAt: queryStamp(at)}
		_, err = tx.ExecContext(ctx, "INSERT INTO reset_attempts(id,account_id,auth_index,binding,credit_id,phase,created_at,updated_at,next_at) VALUES(?,?,?,?,?,'sending',?,?,?)", job.ID, a.ID, a.AuthIndex, a.Binding, c.ID, queryStamp(at), queryStamp(at), queryStamp(at))
		return err
	})
	return job, err
}
func resetWorthwhile(q quota.Snapshot, c policy.ResetCredit, p policy.ResetConfig, at time.Time) bool {
	if !policy.RoutingLabel(c.ID) || c.Status != "available" || c.ResetType != "codexRateLimits" || c.ExpiresAt == nil || *c.ExpiresAt <= at.Unix() || *c.ExpiresAt > at.Unix()+int64(p.ExpirySeconds) {
		return false
	}
	used := 0.0
	eligible := false
	for name, w := range map[string]*core.WindowEvidence{"five_hour": q.FiveHour, "weekly": q.Weekly} {
		stamp := q.Fields[name].ObservedAt
		if stamp.IsZero() {
			stamp = q.ObservedAt
		}
		if stamp.After(at.Add(time.Second)) || at.Sub(stamp) > time.Minute {
			continue
		}
		if w == nil || w.State != core.WindowActive || w.Remaining == nil || w.Remaining.Unit != "percent" || w.Remaining.Value < 0 || w.Remaining.Value > 100 || !w.ResetAt.After(at) {
			continue
		}
		if w.ResetAt.Sub(at) > 5*time.Minute && w.Remaining.Value < 100 {
			eligible = true
			used = max(used, 100-w.Remaining.Value)
		}
	}
	if !eligible || q.ObservedAt.After(at.Add(time.Second)) || at.Sub(q.ObservedAt) > time.Minute {
		return false
	}
	return used >= float64(p.MinUsedPercent) || *c.ExpiresAt-at.Unix() <= int64(p.ForceBeforeSeconds)
}
func (s *Store) PendingReset(ctx context.Context, at time.Time) (ResetAttempt, error) {
	var j ResetAttempt
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.account_id,r.auth_index,r.binding,r.credit_id,r.phase,r.outcome FROM reset_attempts r JOIN accounts a ON a.id=r.account_id JOIN account_bindings b ON b.account_id=a.id AND b.binding=r.binding WHERE r.phase IN ('sending','unknown','verifying') AND r.next_at<=? AND a.auth_index=r.auth_index AND a.status IN ('available','unavailable') ORDER BY r.created_at LIMIT 1`, queryStamp(at)).Scan(&j.ID, &j.AccountID, &j.Account.AuthIndex, &j.Account.Binding, &j.CreditID, &j.Phase, &j.Outcome)
	j.Account.ID = j.AccountID
	j.Account.Status = "available"
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}
func (s *Store) FinishReset(ctx context.Context, j ResetAttempt, phase, outcome, code string, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE reset_attempts SET phase=?,outcome=?,error_code=?,updated_at=?,next_at=? WHERE id=? AND account_id=? AND binding=?", phase, outcome, code, queryStamp(at), queryStamp(at.Add(time.Minute)), j.ID, j.AccountID, j.Account.Binding)
		return err
	})
}
func (s *Store) SaveResetQuota(ctx context.Context, a Account, raw json.RawMessage) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var old sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT a.quota FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.auth_index=? AND b.binding=?", a.ID, a.AuthIndex, a.Binding).Scan(&old)
		if err != nil {
			return err
		}
		merged, changed, err := quota.Merge([]byte(old.String), raw)
		if err != nil || !changed {
			return err
		}
		var q quota.Snapshot
		if json.Unmarshal(merged, &q) != nil || q.Binding != a.Binding {
			return errors.New("account_binding_changed")
		}
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET quota=?,observed_at=? WHERE id=?", string(merged), queryStamp(q.ObservedAt), a.ID)
		return err
	})
}
