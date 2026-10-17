package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

func settingsPayload(revision uint64, config policy.QueryConfig) string {
	raw, _ := json.Marshal(map[string]any{"revision": revision, "config": config})
	return string(raw)
}

func TestQuerySettingsAPIIsValidatedVersionedAndReadOnly(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := &dashboardSource{}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/settings", "", "")
	var initial struct {
		store.QuerySettings
		InstanceID string `json:"instance_id"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &initial) != nil || initial.InstanceID == "" || initial.Revision != 0 || initial.Config.Automatic || initial.Config.FiveHour || initial.Config.Weekly || initial.Config.Delay.Min != 5 || initial.Config.Delay.Max != 60 {
		t.Fatal(response.Code, response.Body.String())
	}
	config := initial.Config
	config.FiveHour = true
	response = request(service, "POST", "/api/v2/settings", settingsPayload(initial.Revision, config), "")
	var saved store.QuerySettings
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Revision != 1 || !saved.Config.FiveHour {
		t.Fatal(response.Code, response.Body.String())
	}
	if response = request(service, "POST", "/api/v2/settings", settingsPayload(0, config), ""); response.Code != 409 || !strings.Contains(response.Body.String(), "settings_conflict") {
		t.Fatal(response.Code, response.Body.String())
	}
	config.Delay.Min, config.Delay.Max = 60, 5
	if response = request(service, "POST", "/api/v2/settings", settingsPayload(saved.Revision, config), ""); response.Code != 400 {
		t.Fatal(response.Code, response.Body.String())
	}
	valid := settingsPayload(saved.Revision, saved.Config)
	for _, body := range []string{`{}`, `{"revision":1}`, valid + ` {}`, strings.Replace(valid, `"config":`, `"private_token":"must-not-export","config":`, 1), `{"revision":1,"config":{"automatic":true}}`} {
		if response = request(service, "POST", "/api/v2/settings", body, ""); response.Code != 400 || strings.Contains(response.Body.String(), "must-not-export") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	current, err := storage.QuerySettings(context.Background())
	if err != nil || current.Revision != saved.Revision || current.Config.Delay.Min != 5 {
		t.Fatal(current, err)
	}
	response = request(service, "GET", "/api/v2/overview", "", "")
	var overview struct {
		Settings     store.QuerySettings `json:"query_settings"`
		Capabilities map[string]bool     `json:"capabilities"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &overview) != nil || !overview.Capabilities["automatic_quota_query"] || !overview.Capabilities["randomized_quota_query"] || overview.Capabilities["model_execution"] || overview.Settings.Revision != saved.Revision {
		t.Fatal(response.Code, response.Body.String())
	}
	if source.accountCalls.Load() != 0 || source.queryCalls.Load() != 0 {
		t.Fatal("settings performed network work")
	}
}

type automaticSource struct {
	accounts atomic.Int32
	queries  atomic.Int32
	catalog  []store.Account
}

func (source *automaticSource) Accounts(context.Context) ([]store.Account, error) {
	source.accounts.Add(1)
	return source.catalog, nil
}
func (source *automaticSource) Query(ctx context.Context, account store.Account) (json.RawMessage, error) {
	source.queries.Add(1)
	stamp := time.Now().UTC()
	raw, err := json.Marshal(map[string]any{"binding": account.Binding, "observed_at": stamp, "read_started_at": stamp, "source": "cpa_wham_usage", "permission": "allowed"})
	return raw, err
}

func TestAutomaticQueriesRunWithoutAnOpenPage(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = storage.SyncInventory(ctx, []store.Account{{ID: "local-fixture", AuthIndex: "index", Binding: "binding", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	settings, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.Automatic = true
	settings.Config.Delay = policy.Delay{}
	if _, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config); err != nil {
		t.Fatal(err)
	}
	source := &automaticSource{}
	service, err := New(storage, source, Options{QueryConcurrency: 2, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	until(t, func() bool {
		jobs, err := storage.RecentJobs(ctx, 10)
		return err == nil && len(jobs) == 1 && jobs[0].Phase == "completed"
	})
	if source.queries.Load() != 1 || source.accounts.Load() != 0 {
		t.Fatal("automatic work required page or repeated inventory", source.queries.Load(), source.accounts.Load())
	}
	view, err := storage.Overview(ctx)
	if err != nil || len(view.Dashboard.RecentJobs) != 1 || view.Dashboard.RecentJobs[0].Trigger != "periodic" {
		t.Fatal(view, err)
	}
}

func TestNewAutomaticDemandDoesNotWaitForAnOlderBatchDeadline(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	account := store.Account{ID: "fixture", AuthIndex: "fixture-index", Binding: "fixture-binding", Status: "available"}
	if err = storage.SyncInventory(ctx, []store.Account{account}); err != nil {
		t.Fatal(err)
	}
	settings, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.Delay = policy.Delay{Min: 60, Max: 60}
	settings, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config)
	if err != nil {
		t.Fatal(err)
	}
	source := &automaticSource{catalog: []store.Account{account}}
	service, err := New(storage, source, Options{QueryConcurrency: 2, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	jobID := accepted(t, service, "", "delayed-manual")
	until(t, func() bool {
		job, err := storage.Job(ctx, jobID)
		return err == nil && len(job.Items) == 1 && job.Items[0].Phase == "queued"
	})
	before, err := storage.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.Automatic = true
	settings.Config.Delay = policy.Delay{}
	if _, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { job, err := storage.Job(ctx, jobID); return err == nil && job.Phase == "completed" })
	after, err := storage.Job(ctx, jobID)
	if err != nil || before.Items[0].ScheduledAt != after.Items[0].ScheduledAt || source.queries.Load() != 1 {
		t.Fatal(before, after, source.queries.Load(), err)
	}
}

func TestDisablingOneSharedRulePreservesTheOtherRuleDeadline(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	account := store.Account{ID: "shared", AuthIndex: "shared-index", Binding: "shared-binding", Status: "available"}
	if err = storage.SyncInventory(ctx, []store.Account{account}); err != nil {
		t.Fatal(err)
	}
	jobID, _, err := storage.CreateQuery(ctx, "initial-evidence", "hash", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.Expand(ctx, job, []store.Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := storage.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC()
	window := map[string]any{"State": "expired", "ResetAt": stamp.Add(-time.Second), "Remaining": map[string]any{"Value": 100, "Unit": "percent"}}
	raw, _ := json.Marshal(map[string]any{"binding": account.Binding, "observed_at": stamp, "read_started_at": stamp, "permission": "allowed", "five_hour": window, "weekly": window})
	if err = storage.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	settings, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.FiveHour, settings.Config.Weekly = true, true
	settings.Config.Delay = policy.Delay{Min: 60, Max: 60}
	settings, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := storage.ScheduleQueries(ctx, time.Now().UTC()); err != nil || created != 1 {
		t.Fatal(created, err)
	}
	view, err := storage.Overview(ctx)
	if err != nil || len(view.Dashboard.ActiveJobs) != 1 {
		t.Fatal(view, err)
	}
	planned := view.Dashboard.ActiveJobs[0]
	settings.Config.Weekly = false
	if _, err = storage.SaveQuerySettings(ctx, settings.Revision, settings.Config); err != nil {
		t.Fatal(err)
	}
	preserved, err := storage.Job(ctx, planned.ID)
	if err != nil || preserved.Phase != "running" || preserved.Trigger != "five_hour_expiry" || preserved.Items[0].Phase != "queued" || preserved.Items[0].ScheduledAt != planned.ScheduledAt {
		t.Fatal(planned, preserved, err)
	}
}
