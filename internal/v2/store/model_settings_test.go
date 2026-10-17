package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"codex-window-refresh/internal/v2/policy"
)

func TestModelSettingsDefaultsNoopPersistenceAndIndependence(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { storage.Close() }()
	initial, err := storage.ModelSettings(ctx)
	if err != nil || initial.Revision != 0 || !reflect.DeepEqual(initial.Config, policy.DefaultModel()) {
		t.Fatal(initial, err)
	}
	var count int
	if err = storage.db.QueryRow("SELECT count(*) FROM model_settings").Scan(&count); err != nil || count != 0 {
		t.Fatal("default read persisted data", count, err)
	}
	config := initial.Config
	config.Phrases = nil
	unchanged, err := storage.SaveModelSettings(ctx, 0, config)
	if err != nil || unchanged.Revision != 0 || unchanged.ViewRevision != initial.ViewRevision || unchanged.Config.Phrases == nil {
		t.Fatal(unchanged, err)
	}
	if err = storage.db.QueryRow("SELECT count(*) FROM model_settings").Scan(&count); err != nil || count != 0 {
		t.Fatal("default save persisted data", count, err)
	}
	queryBefore, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultModel = "fixture-model"
	config.FiveHourModel = "fixture-fast"
	config.FiveHourPrompt = "五小时提示"
	config.WeeklyModel = "fixture-weekly"
	config.WeeklyPrompt = "周提示"
	config.GenerationPrompt = "只生成简短的中文天气问候，不要提出问题。"
	config.PromptMode = "random"
	config.Phrases = []string{"  第一条  ", "第二条"}
	saved, err := storage.SaveModelSettings(ctx, 0, config)
	if err != nil || saved.Revision != 1 || saved.ViewRevision <= initial.ViewRevision || saved.Config.Phrases[0] != "第一条" {
		t.Fatal(saved, err)
	}
	again, err := storage.SaveModelSettings(ctx, 1, config)
	if err != nil || again.Revision != saved.Revision || again.ViewRevision != saved.ViewRevision {
		t.Fatal(again, err)
	}
	if _, err = storage.SaveModelSettings(ctx, 0, config); !errors.Is(err, ErrModelSettingsConflict) {
		t.Fatal(err)
	}
	invalid := config
	invalid.DefaultPrompt = ""
	if _, err = storage.SaveModelSettings(ctx, 1, invalid); !errors.Is(err, policy.ErrInvalidModelSettings) {
		t.Fatal(err)
	}
	queryAfter, err := storage.QuerySettings(ctx)
	if err != nil || queryAfter.Revision != queryBefore.Revision || !reflect.DeepEqual(queryAfter.Config, queryBefore.Config) {
		t.Fatal(queryAfter, err)
	}
	queryAfter.Config.Automatic = true
	if _, err = storage.SaveQuerySettings(ctx, queryAfter.Revision, queryAfter.Config); err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := storage.ModelSettings(ctx)
	if err != nil || restored.Revision != saved.Revision || !reflect.DeepEqual(restored.Config, saved.Config) {
		t.Fatal(restored, err)
	}
	queryRestored, err := storage.QuerySettings(ctx)
	if err != nil || queryRestored.Revision != 1 || !queryRestored.Config.Automatic {
		t.Fatal(queryRestored, err)
	}
}

func TestConcurrentModelSettingsWritersDoNotLoseUpdates(t *testing.T) {
	storage, err := Open(privateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, model := range []string{"fixture-first", "fixture-second"} {
		group.Add(1)
		go func(model string) {
			defer group.Done()
			config := policy.DefaultModel()
			config.DefaultModel = model
			_, err := storage.SaveModelSettings(context.Background(), 0, config)
			errorsSeen <- err
		}(model)
	}
	group.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrModelSettingsConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
}

func TestCorruptModelSettingsAreNotReturned(t *testing.T) {
	storage, err := Open(privateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err = storage.db.Exec(`INSERT INTO model_settings VALUES(1,1,'{"default_prompt":"private-invalid","prompt_mode":"invalid"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ModelSettings(context.Background()); err == nil || err.Error() != "model_settings_unreadable" {
		t.Fatal(err)
	}
}
