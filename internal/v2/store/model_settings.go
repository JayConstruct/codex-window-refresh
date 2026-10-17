package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"codex-window-refresh/internal/v2/policy"
)

var ErrModelSettingsConflict = errors.New("model_settings_conflict")

type ModelSettings struct {
	Revision     uint64             `json:"revision"`
	Config       policy.ModelConfig `json:"config"`
	ViewRevision uint64             `json:"view_revision"`
}

func readModelSettings(ctx context.Context, tx *sql.Tx) (ModelSettings, error) {
	settings := ModelSettings{Config: policy.DefaultModel()}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT revision,config FROM model_settings WHERE singleton=1").Scan(&settings.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if json.Unmarshal([]byte(raw), &settings.Config) != nil || settings.Config.Validate() != nil {
		return settings, errors.New("model_settings_unreadable")
	}
	settings.Config = settings.Config.Normalize()
	return settings, nil
}

func (st *Store) ModelSettings(ctx context.Context) (ModelSettings, error) {
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ModelSettings{}, err
	}
	defer tx.Rollback()
	settings, err := readModelSettings(ctx, tx)
	if err != nil {
		return settings, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&settings.ViewRevision); err != nil {
		return settings, err
	}
	return settings, tx.Commit()
}

func (st *Store) SaveModelSettings(ctx context.Context, expected uint64, config policy.ModelConfig) (ModelSettings, error) {
	if err := config.Validate(); err != nil {
		return ModelSettings{}, err
	}
	config = config.Normalize()
	result := ModelSettings{Config: config}
	err := st.write(ctx, func(tx *sql.Tx) error {
		current, err := readModelSettings(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrModelSettingsConflict
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return err
		}
		old, _ := json.Marshal(current.Config)
		result.Revision = current.Revision
		if string(raw) != string(old) {
			result.Revision++
			if _, err = tx.ExecContext(ctx, "INSERT INTO model_settings VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET revision=excluded.revision,config=excluded.config", result.Revision, string(raw)); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&result.ViewRevision)
	})
	return result, err
}
