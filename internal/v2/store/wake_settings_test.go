package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"codex-window-refresh/internal/v2/policy"
)

func TestWakeSettingsDefaultsNoopPersistenceAndNoHostWork(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { storage.Close() }()
	initial, err := storage.WakeSettings(ctx)
	if err != nil || initial.Revision != 0 || !reflect.DeepEqual(initial.Config, policy.DefaultWake()) {
		t.Fatal(initial, err)
	}
	assertNoWork := func() {
		t.Helper()
		for _, table := range []string{"wake_jobs", "jobs", "query_runs", "prompt_generations", "wake_settings"} {
			var count int
			if err := storage.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected default work/table row %s=%d err=%v", table, count, err)
			}
		}
	}
	assertNoWork()
	unchanged, err := storage.SaveWakeSettings(ctx, initial.Revision, initial.Config)
	if err != nil || unchanged.Revision != 0 || unchanged.ViewRevision != initial.ViewRevision {
		t.Fatal(unchanged, err)
	}
	assertNoWork()
	queryBefore, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	modelBefore, err := storage.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := initial.Config
	config.AllowedHours, config.AccountsPerUser = nil, 0.5
	config.Timezone, config.DailyWindow = "UTC", "23:00-24:00"
	saved, err := storage.SaveWakeSettings(ctx, 0, config)
	if err != nil || saved.Revision != 1 || saved.ViewRevision <= initial.ViewRevision || saved.Config.AllowedHours == nil {
		t.Fatal(saved, err)
	}
	again, err := storage.SaveWakeSettings(ctx, saved.Revision, config)
	if err != nil || again.Revision != saved.Revision || again.ViewRevision != saved.ViewRevision {
		t.Fatal(again, err)
	}
	if _, err = storage.SaveWakeSettings(ctx, 0, config); !errors.Is(err, ErrWakeSettingsConflict) {
		t.Fatalf("stale save err=%v", err)
	}
	invalid := config
	invalid.MaxAccounts = 0
	if _, err = storage.SaveWakeSettings(ctx, 1, invalid); !errors.Is(err, policy.ErrInvalidWake) {
		t.Fatalf("invalid save err=%v", err)
	}
	for _, table := range []string{"wake_jobs", "jobs", "query_runs", "prompt_generations"} {
		var count int
		if err = storage.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("save started work %s=%d err=%v", table, count, err)
		}
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := storage.WakeSettings(ctx)
	if err != nil || restored.Revision != saved.Revision || !reflect.DeepEqual(restored.Config, saved.Config) {
		t.Fatal(restored, err)
	}
	queryAfter, err := storage.QuerySettings(ctx)
	if err != nil || queryAfter.Revision != queryBefore.Revision || !reflect.DeepEqual(queryAfter.Config, queryBefore.Config) {
		t.Fatal(queryAfter, err)
	}
	modelAfter, err := storage.ModelSettings(ctx)
	if err != nil || modelAfter.Revision != modelBefore.Revision || !reflect.DeepEqual(modelAfter.Config, modelBefore.Config) {
		t.Fatal(modelAfter, err)
	}
}

func TestWakeSettingsRequireOnlyModelsForEnabledRules(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*policy.WakeConfig)
		model     func(*policy.ModelConfig)
		wantErr   bool
	}{
		{"five missing", func(config *policy.WakeConfig) { config.FiveHour = true }, nil, true},
		{"weekly missing", func(config *policy.WakeConfig) { config.Weekly = true }, nil, true},
		{"daily missing", func(config *policy.WakeConfig) { config.DailyEnabled = true }, nil, true},
		{"five override", func(config *policy.WakeConfig) { config.FiveHour = true }, func(model *policy.ModelConfig) { model.FiveHourModel = "five-only" }, false},
		{"weekly override", func(config *policy.WakeConfig) { config.Weekly = true }, func(model *policy.ModelConfig) { model.WeeklyModel = "weekly-only" }, false},
		{"daily default", func(config *policy.WakeConfig) { config.DailyEnabled = true }, func(model *policy.ModelConfig) { model.DefaultModel = "default" }, false},
		{"daily cannot borrow override", func(config *policy.WakeConfig) { config.DailyEnabled = true }, func(model *policy.ModelConfig) { model.WeeklyModel = "weekly-only" }, true},
		{"multiple need all", func(config *policy.WakeConfig) { config.FiveHour, config.Weekly = true, true }, func(model *policy.ModelConfig) { model.FiveHourModel = "five-only" }, true},
		{"default covers all", func(config *policy.WakeConfig) {
			config.FiveHour, config.Weekly, config.DailyEnabled = true, true, true
		}, func(model *policy.ModelConfig) { model.DefaultModel = "default" }, false},
		{"master disabled", func(config *policy.WakeConfig) {
			config.Enabled, config.FiveHour, config.Weekly, config.DailyEnabled = false, true, true, true
		}, nil, false},
		{"daily target zero", func(config *policy.WakeConfig) { config.DailyEnabled, config.DailyTarget = true, 0 }, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			storage := openTest(t)
			if test.model != nil {
				model := policy.DefaultModel()
				test.model(&model)
				if _, err := storage.SaveModelSettings(ctx, 0, model); err != nil {
					t.Fatal(err)
				}
			}
			before, err := storage.WakeSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			config := policy.DefaultWake()
			config.Enabled = true
			test.configure(&config)
			saved, err := storage.SaveWakeSettings(ctx, 0, config)
			if test.wantErr {
				if !errors.Is(err, ErrWakeModelMissing) {
					t.Fatalf("missing model err=%v", err)
				}
				after, readErr := storage.WakeSettings(ctx)
				if readErr != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected save mutated settings: %+v err=%v", after, readErr)
				}
			} else if err != nil || saved.Revision != 1 {
				t.Fatal(saved, err)
			}
			var jobs int
			if err := storage.db.QueryRow("SELECT count(*) FROM wake_jobs").Scan(&jobs); err != nil || jobs != 0 {
				t.Fatalf("settings save created jobs=%d err=%v", jobs, err)
			}
		})
	}
}

func TestWakeSettingsCancelOnlyDisabledUnsentAutomaticJobs(t *testing.T) {
	ctx := context.Background()
	storage := openTest(t)
	model := policy.DefaultModel()
	model.DefaultModel = "fixture-model"
	if _, err := storage.SaveModelSettings(ctx, 0, model); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultWake()
	config.Enabled, config.FiveHour, config.Weekly, config.DailyEnabled = true, true, true, true
	saved, err := storage.SaveWakeSettings(ctx, 0, config)
	if err != nil {
		t.Fatal(err)
	}
	phases := []string{"queued", "checking", "prepared", "sending", "verifying", "unknown", "completed", "failed"}
	reasons := []string{"manual", "five_hour_reset", "weekly_reset", "daily"}
	for _, reason := range reasons {
		for _, phase := range phases {
			id := reason + "-" + phase
			if _, err := storage.db.Exec(`INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at,hold_reason) VALUES(?,?,?,'binding',?,'fixture-cycle',?,?,?,?, 'fixture-hold')`, id, id, "index-"+id, reason, phase, now(), now(), now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	config.FiveHour = false
	saved, err = storage.SaveWakeSettings(ctx, saved.Revision, config)
	if err != nil {
		t.Fatal(err)
	}
	check := func(masterDisabled bool) {
		t.Helper()
		for _, reason := range reasons {
			for _, phase := range phases {
				id := reason + "-" + phase
				var actual, code, hold string
				if err := storage.db.QueryRow("SELECT phase,error_code,hold_reason FROM wake_jobs WHERE id=?", id).Scan(&actual, &code, &hold); err != nil {
					t.Fatal(err)
				}
				unsent := phase == "queued" || phase == "checking" || phase == "prepared"
				cancelled := reason != "manual" && unsent && (reason == "five_hour_reset" || masterDisabled)
				if cancelled {
					if actual != "cancelled" || code != "wake_rule_disabled" || hold != "" {
						t.Fatalf("not cancelled safely: %s=%s %s %s", id, actual, code, hold)
					}
				} else if actual != phase || code != "" || hold != "fixture-hold" {
					t.Fatalf("unrelated work changed: %s=%s %s %s", id, actual, code, hold)
				}
			}
		}
	}
	check(false)
	config.Enabled = false
	if _, err = storage.SaveWakeSettings(ctx, saved.Revision, config); err != nil {
		t.Fatal(err)
	}
	check(true)
}

func TestConcurrentWakeSettingsWritersRejectLostUpdates(t *testing.T) {
	storage := openTest(t)
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, target := range []int{2, 3} {
		group.Add(1)
		go func(target int) {
			defer group.Done()
			config := policy.DefaultWake()
			config.DailyTarget = target
			_, err := storage.SaveWakeSettings(context.Background(), 0, config)
			results <- err
		}(target)
	}
	group.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrWakeSettingsConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(fmt.Sprintf("success=%d conflict=%d", success, conflict))
	}
}
