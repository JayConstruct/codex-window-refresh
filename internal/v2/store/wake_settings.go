package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"codex-window-refresh/internal/v2/policy"
)

var ErrWakeSettingsConflict = errors.New("wake_settings_conflict")
var ErrWakeModelMissing = errors.New("wake_model_missing")
var ErrWakeAccount = errors.New("wake_account_unavailable")
var ErrInvalidWakeRequest = errors.New("invalid_wake_request")

type WakeSettings struct {
	Revision     uint64            `json:"revision"`
	Config       policy.WakeConfig `json:"config"`
	ViewRevision uint64            `json:"view_revision"`
}

func readWakeSettings(ctx context.Context, tx *sql.Tx) (WakeSettings, error) {
	settings := WakeSettings{Config: policy.DefaultWake()}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT revision,config FROM wake_settings WHERE singleton=1").Scan(&settings.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if json.Unmarshal([]byte(raw), &settings.Config) != nil || settings.Config.Validate() != nil {
		return settings, errors.New("wake_settings_unreadable")
	}
	settings.Config = settings.Config.Normalize()
	return settings, nil
}

func (st *Store) WakeSettings(ctx context.Context) (WakeSettings, error) {
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WakeSettings{}, err
	}
	defer tx.Rollback()
	settings, err := readWakeSettings(ctx, tx)
	if err != nil {
		return settings, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&settings.ViewRevision); err != nil {
		return settings, err
	}
	return settings, tx.Commit()
}

func (st *Store) SaveWakeSettings(ctx context.Context, expected uint64, config policy.WakeConfig) (WakeSettings, error) {
	if config.Validate() != nil {
		return WakeSettings{}, policy.ErrInvalidWake
	}
	config = config.Normalize()
	result := WakeSettings{Config: config}
	err := st.write(ctx, func(tx *sql.Tx) error {
		current, err := readWakeSettings(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrWakeSettingsConflict
		}
		if config.Enabled {
			model, err := readModelSettings(ctx, tx)
			if err != nil {
				return err
			}
			for _, reason := range []string{"five_hour_reset", "weekly_reset", "daily"} {
				selected, _ := policy.WakeModel(model.Config, reason)
				if config.RuleEnabled(reason) && strings.TrimSpace(selected) == "" {
					return ErrWakeModelMissing
				}
			}
		}
		previous, _ := json.Marshal(current.Config)
		raw, _ := json.Marshal(config)
		result.Revision = current.Revision
		if string(previous) != string(raw) {
			result.Revision++
			if _, err = tx.ExecContext(ctx, "INSERT INTO wake_settings VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET revision=excluded.revision,config=excluded.config", result.Revision, string(raw)); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE wake_jobs SET phase='cancelled',hold_reason='',error_code='wake_rule_disabled',updated_at=? WHERE phase IN ('queued','checking','prepared') AND reason!='manual' AND
 (?=0 OR (reason='five_hour_reset' AND ?=0) OR (reason='weekly_reset' AND ?=0) OR (reason='daily' AND ?=0))`, now(), config.Enabled, config.FiveHour, config.Weekly, config.DailyEnabled && config.DailyTarget > 0); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&result.ViewRevision)
	})
	return result, err
}
