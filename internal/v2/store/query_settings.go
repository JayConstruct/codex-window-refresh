package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"codex-window-refresh/internal/v2/policy"
)

var ErrSettingsConflict = errors.New("settings_conflict")

type QuerySettings struct {
	Revision     uint64             `json:"revision"`
	Config       policy.QueryConfig `json:"config"`
	ViewRevision uint64             `json:"view_revision"`
}

func readQuerySettings(ctx context.Context, tx *sql.Tx) (QuerySettings, error) {
	settings := QuerySettings{Config: policy.DefaultQuery()}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT revision,config FROM query_settings WHERE singleton=1").Scan(&settings.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if json.Unmarshal([]byte(raw), &settings.Config) != nil || settings.Config.Validate() != nil {
		return settings, policy.ErrInvalid
	}
	return settings, nil
}

func (st *Store) QuerySettings(ctx context.Context) (QuerySettings, error) {
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return QuerySettings{}, err
	}
	defer tx.Rollback()
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return settings, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&settings.ViewRevision); err != nil {
		return settings, err
	}
	return settings, tx.Commit()
}

func (st *Store) SaveQuerySettings(ctx context.Context, expected uint64, config policy.QueryConfig) (QuerySettings, error) {
	if err := config.Validate(); err != nil {
		return QuerySettings{}, err
	}
	if config.ExcludedHours == nil {
		config.ExcludedHours = []string{}
	}
	result := QuerySettings{Config: config}
	err := st.write(ctx, func(tx *sql.Tx) error {
		current, err := readQuerySettings(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrSettingsConflict
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return err
		}
		old, _ := json.Marshal(current.Config)
		result.Revision = current.Revision
		if string(raw) != string(old) {
			result.Revision++
			if _, err = tx.ExecContext(ctx, "INSERT INTO query_settings VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET revision=excluded.revision,config=excluded.config", result.Revision, string(raw)); err != nil {
				return err
			}
			if err = cancelDisabledQueries(ctx, tx, config); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&result.ViewRevision)
	})
	return result, err
}
