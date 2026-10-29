package store

import (
	"context"
	"reflect"
	"testing"

	"codex-window-refresh/internal/v2/policy"
)

func TestWakeVersionSevenMigrationPreservesGenerationAndSettingsWithWakeOff(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { storage.Close() }()
	models := policy.DefaultModel()
	models.DefaultModel, models.GenerationPrompt = "legacy-default-model", "生成简洁的中文问候。"
	models.FiveHourModel, models.WeeklyModel = "legacy-five-model", "legacy-weekly-model"
	models.PromptMode, models.Phrases = "random", []string{"你好。", "今天天气不错。"}
	modelBefore, err := storage.SaveModelSettings(ctx, 0, models)
	if err != nil {
		t.Fatal(err)
	}
	queries := policy.DefaultQuery()
	queries.Weekly, queries.Delay = true, policy.Delay{Min: 45, Max: 120}
	queryBefore, err := storage.SaveQuerySettings(ctx, 0, queries)
	if err != nil {
		t.Fatal(err)
	}
	request := GenerationRequest{AccountID: "legacy-antigravity", Model: "legacy-generation-model", Count: 1, Prompt: "只输出一句中文候选。"}
	hash, err := generationRequestHash("legacy-generation-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.db.Exec(`INSERT INTO prompt_generations(id,idempotency_key,request_hash,account_id,auth_index,binding,provider,model,count,prompt,phase,candidates,created_at,updated_at)
		VALUES('legacy-generation','legacy-generation-key',?,?,'legacy-index','legacy-binding','antigravity',?,1,?,'completed','["旧版候选。"]',?,?)`, hash, request.AccountID, request.Model, request.Prompt, now(), now()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"wake_responses", "wake_cursor", "wake_baselines", "wake_cycles", "wake_jobs", "wake_settings"} {
		if _, err = storage.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = storage.db.Exec("UPDATE schema_version SET version=7"); err != nil {
		t.Fatal(err)
	}
	var wakeTables int
	if err = storage.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'wake_%'").Scan(&wakeTables); err != nil || wakeTables != 0 {
		t.Fatalf("fixture retained wake tables=%d err=%v", wakeTables, err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = storage.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 11 {
		t.Fatalf("migrated schema=%d err=%v", version, err)
	}
	wakes, err := storage.WakeSettings(ctx)
	if err != nil || wakes.Revision != 0 || !reflect.DeepEqual(wakes.Config, policy.DefaultWake()) || wakes.Config.Enabled {
		t.Fatalf("migration enabled waking: %+v err=%v", wakes, err)
	}
	for _, table := range []string{"wake_jobs", "wake_settings", "wake_cycles", "wake_baselines"} {
		var count int
		if err = storage.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("migration started work %s=%d err=%v", table, count, err)
		}
	}
	modelAfter, err := storage.ModelSettings(ctx)
	if err != nil || modelAfter.Revision != modelBefore.Revision || !reflect.DeepEqual(modelAfter.Config, modelBefore.Config) {
		t.Fatal(modelAfter, err)
	}
	queryAfter, err := storage.QuerySettings(ctx)
	if err != nil || queryAfter.Revision != queryBefore.Revision || !reflect.DeepEqual(queryAfter.Config, queryBefore.Config) {
		t.Fatal(queryAfter, err)
	}
	generation, found, err := storage.LookupGeneration(ctx, "legacy-generation-key", request)
	if err != nil || !found || generation.ID != "legacy-generation" || generation.Phase != "completed" || generation.Prompt != request.Prompt || generation.Model != request.Model || generation.Account.Provider != "antigravity" || generation.Account.AuthIndex != "legacy-index" || generation.Account.Binding != "legacy-binding" || !reflect.DeepEqual(generation.Candidates, []string{"旧版候选。"}) {
		t.Fatal(generation, found, err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := storage.WakeSettings(ctx)
	if err != nil || again.Revision != 0 || !reflect.DeepEqual(again.Config, policy.DefaultWake()) {
		t.Fatalf("reopen changed defaults: %+v err=%v", again, err)
	}
}
