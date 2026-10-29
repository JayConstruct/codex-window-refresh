package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-window-refresh/internal/v2/policy"
)

func TestVersionFiveMigrationPreservesGenerationSnapshotsAndKeys(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, "SELECT 11 WHERE NOT EXISTS", "SELECT 5 WHERE NOT EXISTS", 1)
	old = strings.Replace(old, "provider TEXT NOT NULL DEFAULT 'codex', ", "", 1)
	old = strings.Replace(old, "prompt TEXT NOT NULL DEFAULT '', ", "", 1)
	if _, err = database.Exec(old); err != nil {
		t.Fatal(err)
	}
	var providerColumns int
	if err = database.QueryRow("SELECT count(*) FROM pragma_table_info('prompt_generations') WHERE name='provider'").Scan(&providerColumns); err != nil || providerColumns != 0 {
		t.Fatal("fixture is not schema 5", providerColumns, err)
	}
	request := GenerationRequest{AccountID: "schema-five-account", Model: "legacy-model", Count: 1}
	for _, phase := range []string{"queued", "running", "unknown", "completed"} {
		hash, hashErr := generationRequestHash(phase, request)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		if _, err = database.Exec("INSERT INTO prompt_generations(id,idempotency_key,request_hash,account_id,auth_index,binding,model,count,phase,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?,?)", phase, phase, hash, request.AccountID, "legacy-index", "legacy-binding", request.Model, phase, now(), now()); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for _, phase := range []string{"queued", "running", "unknown", "completed"} {
		generation, existing, err := storage.LookupGeneration(ctx, phase, request)
		if err != nil || !existing || generation.ID != phase || generation.Phase != phase || generation.Account.Provider != "codex" || generation.Account.AuthIndex != "legacy-index" || generation.Account.Binding != "legacy-binding" {
			t.Fatal(generation, existing, err)
		}
	}
	if err = storage.RecoverGenerations(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, existing, err := storage.CreateGeneration(ctx, "running", request)
	if err != nil || !existing || recovered.Phase != "unknown" || recovered.ErrorCode != "generation_interrupted" {
		t.Fatal(recovered, existing, err)
	}
	if err = storage.SyncInventory(ctx, []Account{{ID: request.AccountID, AuthIndex: "legacy-index", Binding: "legacy-binding", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimGeneration(ctx)
	if err != nil || claimed.ID != "queued" || claimed.Account.Provider != "codex" || claimed.Phase != "running" {
		t.Fatal(claimed, err)
	}
	var version, columns int
	if err = storage.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 11 {
		t.Fatal(version, err)
	}
	if err = storage.db.QueryRow("SELECT count(*) FROM pragma_table_info('prompt_generations') WHERE name='provider'").Scan(&columns); err != nil || columns != 1 {
		t.Fatal(columns, err)
	}
}

func TestVersionSixMigrationKeepsProviderAndLegacyGenerationHash(t *testing.T) {
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, "SELECT 11 WHERE NOT EXISTS", "SELECT 6 WHERE NOT EXISTS", 1)
	old = strings.Replace(old, "prompt TEXT NOT NULL DEFAULT '', ", "", 1)
	if _, err = database.Exec(old); err != nil {
		t.Fatal(err)
	}
	request := GenerationRequest{AccountID: "antig", Model: "fixture-model", Count: 1}
	hash, err := generationRequestHash("old-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec("INSERT INTO prompt_generations(id,idempotency_key,request_hash,account_id,auth_index,binding,provider,model,count,phase,created_at,updated_at) VALUES('original','old-key',?,'antig','index','binding','antigravity','fixture-model',1,'running',?,?)", hash, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`INSERT INTO model_settings VALUES(1,9,'{"default_model":"legacy-model","default_prompt":"legacy prompt","five_hour_model":"","five_hour_prompt":"","weekly_model":"","weekly_prompt":"","prompt_mode":"fixed","phrases":[]}')`); err != nil {
		t.Fatal(err)
	}
	database.Close()
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	if err = storage.RecoverGenerations(ctx); err != nil {
		t.Fatal(err)
	}
	previous, exists, err := storage.LookupGeneration(ctx, "old-key", request)
	if err != nil || !exists || previous.ID != "original" || previous.Phase != "unknown" || previous.Account.Provider != "antigravity" || previous.Prompt != "" {
		t.Fatal(previous, exists, err)
	}
	settings, err := storage.ModelSettings(ctx)
	if err != nil || settings.Revision != 9 || settings.Config.DefaultModel != "legacy-model" || settings.Config.GenerationPrompt != "" {
		t.Fatal(settings, err)
	}
	var columns int
	if err = storage.db.QueryRow("SELECT count(*) FROM pragma_table_info('prompt_generations') WHERE name='prompt'").Scan(&columns); err != nil || columns != 1 {
		t.Fatal(columns, err)
	}
}

func TestVersionOneMigratesWithoutLosingJobOrIdempotency(t *testing.T) {
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(schema, "CREATE INDEX IF NOT EXISTS jobs_retention")[0]
	old = strings.Replace(old, "SELECT 11 WHERE NOT EXISTS", "SELECT 1 WHERE NOT EXISTS", 1)
	if _, err = db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES('original','key','hash','a','queued',?,?)", now(), now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, existing, err := s.Create(context.Background(), "key", "hash", "a")
	if err != nil || !existing || id != "original" {
		t.Fatal(id, existing, err)
	}
	var version int
	if err = s.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 11 {
		t.Fatal(version, err)
	}
}

func TestVersionTwoMigrationKeepsQuotaAndAddsBindingState(t *testing.T) {
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(schema, "CREATE TABLE IF NOT EXISTS account_bindings")[0]
	old = strings.Replace(old, "SELECT 11 WHERE NOT EXISTS", "SELECT 2 WHERE NOT EXISTS", 1)
	if _, err = db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO accounts(id,auth_index,status,quota,updated_at) VALUES('a','index','available','{"permission":"allowed"}',?)`, now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	accounts, err := s.Accounts(context.Background(), "", 10)
	if err != nil || len(accounts) != 1 || string(accounts[0].Quota) != `{"permission":"allowed"}` {
		t.Fatal(accounts, err)
	}
	if _, err = s.Overview(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVersionThreeMigrationKeepsLegacyQueueBindingsAndReceipts(t *testing.T) {
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account := Account{ID: "legacy-account", AuthIndex: "legacy-index", Binding: "legacy-binding", Status: "available"}
	if err = storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	jobID, _, err := storage.Create(ctx, "legacy-request", "legacy-hash", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.Expand(ctx, job, []Account{account}); err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite3", filepath.Join(dir, "backend.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`DROP TRIGGER query_timer_outcome;
 DROP TABLE query_schedule; DROP TABLE query_plans; DROP TABLE query_settings;
 DROP TABLE query_run_bindings; DROP TABLE query_baselines; DROP TABLE query_timers;
 DROP TABLE query_scheduler_cursor; DROP TABLE model_settings; DROP TABLE prompt_generations; UPDATE schema_version SET version=3`)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.Close()
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	replayed, existing, err := storage.CreateQuery(ctx, "legacy-request", "legacy-hash", account.ID)
	if err != nil || !existing || replayed != jobID {
		t.Fatal(replayed, existing, err)
	}
	settings, err := storage.QuerySettings(ctx)
	if err != nil || settings.Revision != 0 || settings.Config.Automatic || settings.Config.Delay.Min != 5 {
		t.Fatal(settings, err)
	}
	run, err := storage.Claim(ctx)
	if err != nil || run.Account.Binding != account.Binding || run.Account.AuthIndex != account.AuthIndex {
		t.Fatal(run, err)
	}
}

func TestVersionFourMigrationPreservesQueryConfigurationAndSchedule(t *testing.T) {
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account := Account{ID: "schema-four", AuthIndex: "schema-four-index", Binding: "schema-four-binding", Status: "available"}
	if err = storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	settings, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.Delay = policy.Delay{Min: 300, Max: 300}
	settings.Config.Weekly = true
	settings, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := storage.CreateQuery(ctx, "schema-four-receipt", "schema-four-hash", "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.Expand(ctx, job, []Account{account}); err != nil {
		t.Fatal(err)
	}
	before, err := storage.Job(ctx, jobID)
	if err != nil || len(before.Items) != 1 || before.Items[0].ScheduledAt == "" {
		t.Fatal(before, err)
	}
	if _, err = storage.db.Exec("DROP TABLE model_settings; DROP TABLE prompt_generations; UPDATE schema_version SET version=4"); err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var version int
	if err = storage.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 11 {
		t.Fatal(version, err)
	}
	models, err := storage.ModelSettings(ctx)
	if err != nil || models.Revision != 0 || models.Config.DefaultPrompt != policy.DefaultModel().DefaultPrompt {
		t.Fatal(models, err)
	}
	queries, err := storage.QuerySettings(ctx)
	if err != nil || queries.Revision != settings.Revision || queries.Config.Delay.Min != 300 || !queries.Config.Weekly {
		t.Fatal(queries, err)
	}
	after, err := storage.Job(ctx, jobID)
	if err != nil || after.Items[0].ScheduledAt != before.Items[0].ScheduledAt || after.Items[0].Phase != "queued" {
		t.Fatal(before, after, err)
	}
	replayed, existing, err := storage.CreateQuery(ctx, "schema-four-receipt", "schema-four-hash", "")
	if err != nil || !existing || replayed != jobID {
		t.Fatal(replayed, existing, err)
	}
	accounts, err := storage.Accounts(ctx, "", 10)
	if err != nil || len(accounts) != 1 || accounts[0].AuthIndex != account.AuthIndex {
		t.Fatal(accounts, err)
	}
	var binding string
	if err = storage.db.QueryRow("SELECT binding FROM account_bindings WHERE account_id=?", account.ID).Scan(&binding); err != nil || binding != account.Binding {
		t.Fatal(binding, err)
	}
}
