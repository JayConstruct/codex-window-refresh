package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
	"codex-window-refresh/internal/v2/store"
)

type wakeSource struct {
	dashboardSource
	unavailable atomic.Bool
	wakeCalls   atomic.Int32
	execute     func(context.Context, store.Account, string, string) error
}

func (source *wakeSource) WakeAvailable() bool { return !source.unavailable.Load() }

func (source *wakeSource) ExecuteWake(ctx context.Context, account store.Account, model, prompt string) error {
	source.wakeCalls.Add(1)
	if source.execute == nil {
		return errors.New("unexpected model call")
	}
	return source.execute(ctx, account, model, prompt)
}

func wakeService(t *testing.T, source *wakeSource) (*Service, *store.Store) {
	t.Helper()
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	ctx := context.Background()
	if err = storage.SyncInventory(ctx, []store.Account{{ID: "wake-account", AuthIndex: "wake-index", Binding: "private-binding", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	models, err := storage.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	models.Config.DefaultModel = "private-model"
	models.Config.DefaultPrompt = "private prompt content"
	if _, err = storage.SaveModelSettings(ctx, models.Revision, models.Config); err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return service, storage
}

func wakeSettingsBody(revision uint64, config policy.WakeConfig) string {
	raw, _ := json.Marshal(map[string]any{"revision": revision, "config": config})
	return string(raw)
}

const wakeBody = `{"account_id":"wake-account"}`

func acceptWake(t *testing.T, service *Service, key string) store.WakeJob {
	t.Helper()
	response := request(service, "POST", "/api/v2/wake-jobs", wakeBody, key)
	var job store.WakeJob
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &job) != nil || job.ID == "" || job.InstanceID != service.instance {
		t.Fatal(response.Code, response.Body.String())
	}
	return job
}

func assertNoWakeReadsOrSends(t *testing.T, source *wakeSource) {
	t.Helper()
	if source.wakeCalls.Load() != 0 || source.accountCalls.Load() != 0 || source.queryCalls.Load() != 0 {
		t.Fatal("control API performed host work", source.wakeCalls.Load(), source.accountCalls.Load(), source.queryCalls.Load())
	}
}

func TestWakeSettingsStrictVersionedAndNoSynchronousModelCalls(t *testing.T) {
	source := &wakeSource{}
	service, storage := wakeService(t, source)
	response := request(service, "GET", "/api/v2/wake-settings", "", "")
	var initial struct {
		store.WakeSettings
		InstanceID string `json:"instance_id"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &initial) != nil || initial.InstanceID != service.instance || initial.Config.Enabled || initial.Revision != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	config := initial.Config
	config.Enabled, config.FiveHour = true, true
	response = request(service, "POST", "/api/v2/wake-settings", wakeSettingsBody(0, config), "")
	var saved store.WakeSettings
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Revision != 1 || !saved.Config.Enabled || !saved.Config.FiveHour {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/wake-settings", wakeSettingsBody(0, config), "")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "wake_settings_conflict") {
		t.Fatal(response.Code, response.Body.String())
	}
	valid := wakeSettingsBody(saved.Revision, config)
	for _, body := range []string{
		`{}`, `null`, `{"revision":1}`, `{"config":{}}`, `{"revision":1,"config":null}`,
		valid + ` {}`, strings.Replace(valid, `"config":`, `"secret":"not-exported","config":`, 1),
		strings.Replace(valid, `"enabled":true`, `"enabled":true,"model":"not-accepted"`, 1),
		string([]byte{'{', '"', 255, '"', ':', '1', '}'}), strings.Repeat(" ", 16385) + valid,
	} {
		response = request(service, "POST", "/api/v2/wake-settings", body, "")
		if response.Code != 400 || strings.Contains(response.Body.String(), "not-exported") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	invalid := config
	invalid.FiveHour = false
	response = request(service, "POST", "/api/v2/wake-settings", wakeSettingsBody(saved.Revision, invalid), "")
	if response.Code != 400 || !strings.Contains(response.Body.String(), "invalid_wake_settings") {
		t.Fatal(response.Code, response.Body.String())
	}
	current, err := storage.WakeSettings(context.Background())
	if err != nil || current.Revision != saved.Revision || !current.Config.FiveHour {
		t.Fatal(current, err)
	}
	response = request(service, "GET", "/api/v2/overview", "", "")
	var view struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Capabilities["model_execution"] != true || view.Capabilities["automatic_wake"] != true || view.Capabilities["wake_settings"] != true {
		t.Fatal(response.Code, response.Body.String())
	}
	assertNoWakeReadsOrSends(t, source)
}

func TestWakeJobsStrictRedactedAndIdempotentBeforeHostCapability(t *testing.T) {
	source := &wakeSource{}
	service, storage := wakeService(t, source)
	for _, body := range []string{
		`{}`, `null`, `{"account_id":""}`, `{"account_id":"bad\nid"}`, wakeBody + `{}`,
		`{"account_id":"wake-account","provider":"codex"}`, `{"account_id":"wake-account","reason":"manual"}`,
		`{"account_id":"wake-account","model":"test"}`, `{"account_id":"wake-account","prompt":"secret-content"}`,
		`{"account_id":"wake-account","auth_index":"secret-index"}`, `{"account_id":"wake-account","binding":"secret-binding"}`,
		string([]byte{'{', '"', 255, '"', ':', '1', '}'}), strings.Repeat(" ", 4097) + wakeBody,
	} {
		response := request(service, "POST", "/api/v2/wake-jobs", body, "invalid")
		if response.Code != 400 || strings.Contains(response.Body.String(), "secret") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	for _, key := range []string{"", "contains space", strings.Repeat("x", 129)} {
		response := request(service, "POST", "/api/v2/wake-jobs", wakeBody, key)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "idempotency_key_required") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	initial := acceptWake(t, service, "durable-wake")
	if initial.Phase != "queued" || initial.Reason != "manual" || initial.ScheduledAt == "" {
		t.Fatal(initial)
	}
	source.unavailable.Store(true)
	if err := storage.SyncInventory(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	duplicate := acceptWake(t, service, "durable-wake")
	if duplicate.ID != initial.ID {
		t.Fatal(duplicate)
	}
	response := request(service, "POST", "/api/v2/wake-jobs", `{"account_id":"another-account"}`, "durable-wake")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "idempotency_conflict") {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/wake-jobs", wakeBody, "new-wake")
	if response.Code != 503 || !strings.Contains(response.Body.String(), "wake_unavailable") {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, path := range []string{"/api/v2/wake-jobs?job_id=" + initial.ID, "/api/v2/wake-jobs", "/api/v2/wake-jobs?limit=1"} {
		response = request(service, "GET", path, "", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), initial.ID) || !strings.Contains(response.Body.String(), service.instance) {
			t.Fatal(response.Code, response.Body.String())
		}
		for _, private := range []string{"private", "auth_index", "binding", "policy_revision", "model_revision", "prompt", "model", "query_gate", "request_hash", "durable-wake"} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatal("private job field leaked", private, response.Body.String())
			}
		}
	}
	for _, path := range []string{"/api/v2/wake-jobs?job_id=", "/api/v2/wake-jobs?limit=0", "/api/v2/wake-jobs?limit=101", "/api/v2/wake-jobs?limit=bad"} {
		response = request(service, "GET", path, "", "")
		if response.Code != 400 {
			t.Fatal(path, response.Code, response.Body.String())
		}
	}
	response = request(service, "GET", "/api/v2/wake-jobs?job_id=missing", "", "")
	if response.Code != 404 {
		t.Fatal(response.Code, response.Body.String())
	}
	assertNoWakeReadsOrSends(t, source)
}

func TestWakeMissingModelAndUnavailableAccountAreExplicit(t *testing.T) {
	source := &wakeSource{}
	service, storage := wakeService(t, source)
	ctx := context.Background()
	models, err := storage.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	models.Config.DefaultModel = ""
	if _, err = storage.SaveModelSettings(ctx, models.Revision, models.Config); err != nil {
		t.Fatal(err)
	}
	response := request(service, "POST", "/api/v2/wake-jobs", wakeBody, "model-missing")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "wake_model_missing") {
		t.Fatal(response.Code, response.Body.String())
	}
	settings := policy.DefaultWake()
	settings.Enabled, settings.Weekly = true, true
	response = request(service, "POST", "/api/v2/wake-settings", wakeSettingsBody(0, settings), "")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "wake_model_missing") {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/wake-jobs", `{"account_id":"not-found"}`, "missing")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "wake_account_not_found") {
		t.Fatal(response.Code, response.Body.String())
	}
	assertNoWakeReadsOrSends(t, source)
}

type wakeWithoutCapability struct {
	dashboardSource
	wakeCalls atomic.Int32
}

func (source *wakeWithoutCapability) ExecuteWake(context.Context, store.Account, string, string) error {
	source.wakeCalls.Add(1)
	return nil
}

func TestWakeRequiresExplicitCapabilityAndKeepsSettingsAvailable(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := &wakeWithoutCapability{}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/wake-settings", "/api/v2/wake-jobs", "/api/v2/overview"} {
		response := request(service, "GET", path, "", "")
		if response.Code != 200 {
			t.Fatal(path, response.Code, response.Body.String())
		}
		if path == "/api/v2/wake-jobs" && !strings.Contains(response.Body.String(), `"jobs":[]`) {
			t.Fatal(response.Body.String())
		}
		if path == "/api/v2/overview" && (!strings.Contains(response.Body.String(), `"model_execution":false`) || !strings.Contains(response.Body.String(), `"automatic_wake":false`) || !strings.Contains(response.Body.String(), `"wake_settings":true`)) {
			t.Fatal(response.Body.String())
		}
	}
	response := request(service, "POST", "/api/v2/wake-jobs", wakeBody, "unavailable")
	if response.Code != 503 {
		t.Fatal(response.Code, response.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	time.Sleep(250 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wake lifecycle did not stop")
	}
	if source.wakeCalls.Load() != 0 || source.accountCalls.Load() != 0 || source.queryCalls.Load() != 0 {
		t.Fatal("legacy source dispatched wake work")
	}
}

func TestWakeOutcomeIsConservativeAndSanitized(t *testing.T) {
	for _, fixture := range []struct{ code, phase, public string }{
		{"", "verifying", ""},
		{"wake_delivery_unknown", "unknown", "wake_outcome_unknown"},
		{"wake_response_invalid", "unknown", "wake_outcome_unknown"},
		{"private credential failure", "unknown", "wake_outcome_unknown"},
		{"account_binding_changed", "failed", "wake_binding_changed"},
		{"account_disabled", "failed", "wake_account_disabled"},
		{"account_not_found", "failed", "wake_account_not_found"},
		{"account_identity_ambiguous", "failed", "wake_account_ambiguous"},
		{"account_identity_unavailable", "failed", "wake_account_unavailable"},
		{"wake_account_unavailable", "failed", "wake_account_unavailable"},
		{"wake_account_not_found", "failed", "wake_account_not_found"},
		{"wake_account_disabled", "failed", "wake_account_disabled"},
		{"wake_host_unavailable", "failed", "wake_host_unavailable"},
		{"wake_inventory_stale", "failed", "wake_inventory_stale"},
		{"wake_account_ambiguous", "failed", "wake_account_ambiguous"},
		{"wake_binding_missing", "failed", "wake_binding_missing"},
		{"wake_binding_changed", "failed", "wake_binding_changed"},
		{"wake_not_supported", "failed", "wake_request_rejected"},
		{"generation_not_supported", "failed", "wake_request_rejected"},
		{"wake_request_invalid", "failed", "wake_request_rejected"},
	} {
		var err error
		if fixture.code != "" {
			err = errors.New(fixture.code)
		}
		phase, code := wakeOutcome(err)
		if phase != fixture.phase || code != fixture.public {
			t.Fatal(fixture.code, phase, code)
		}
	}
}

func TestWakeAPIReturnsOnlyVerifiedAccountClassifications(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		accounts []store.Account
		failed   bool
		code     string
	}{
		{"deleted", nil, false, "wake_account_not_found"},
		{"disabled", []store.Account{{ID: "wake-account", AuthIndex: "private-index", Binding: "private-binding", Status: "disabled"}}, false, "wake_account_disabled"},
		{"unavailable", []store.Account{{ID: "wake-account", AuthIndex: "private-index", Binding: "private-binding", Status: "unavailable"}}, false, "wake_host_unavailable"},
		{"missing binding", []store.Account{{ID: "wake-account", AuthIndex: "private-index", Status: "available"}}, false, "wake_binding_missing"},
		{"ambiguous", []store.Account{{ID: "wake-account", AuthIndex: "private-index", Binding: "private-binding", Status: "available"}, {ID: "duplicate", AuthIndex: "private-index", Binding: "other-private-binding", Status: "available"}}, false, "wake_account_ambiguous"},
		{"unresolved identity", []store.Account{{ID: "wake-account", Status: "binding_unavailable"}}, false, "wake_account_unavailable"},
		{"unavailable inventory", nil, true, "wake_inventory_stale"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := &wakeSource{}
			service, storage := wakeService(t, source)
			var err error
			if fixture.failed {
				err = storage.InventoryFailed(context.Background())
			} else {
				err = storage.SyncInventory(context.Background(), fixture.accounts)
			}
			if err != nil {
				t.Fatal(err)
			}
			response := request(service, "POST", "/api/v2/wake-jobs", wakeBody, "classification")
			if response.Code != 409 || !strings.Contains(response.Body.String(), fixture.code) || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("unsafe or imprecise API error", response.Code, response.Body.String())
			}
			assertNoWakeReadsOrSends(t, source)
		})
	}
}

type workerWakeSource struct {
	wakeSource
	activateAfterSend bool
	catalog           []store.Account
}

func (source *workerWakeSource) Accounts(context.Context) ([]store.Account, error) {
	source.accountCalls.Add(1)
	return append([]store.Account{}, source.catalog...), nil
}

func (source *workerWakeSource) Query(_ context.Context, account store.Account) (json.RawMessage, error) {
	source.queryCalls.Add(1)
	at := time.Now().UTC()
	permission := core.PermissionAllowed
	short := &core.WindowEvidence{Kind: core.FiveHour, State: core.WindowInactive, Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}}
	if source.activateAfterSend && source.wakeCalls.Load() > 0 {
		short.State, short.ResetAt = core.WindowActive, at.Add(5*time.Hour)
		short.Remaining.Value = 99
	}
	return json.Marshal(quota.Snapshot{
		Binding: account.Binding, Source: "cpa_wham_usage", ObservedAt: at, ReadStartedAt: at, Permission: &permission,
		FiveHour: short, Weekly: &core.WindowEvidence{Kind: core.Weekly, State: core.WindowActive, ResetAt: at.Add(7 * 24 * time.Hour), Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}},
	})
}

func prepareWakeWorker(t *testing.T, source *workerWakeSource) (*Service, *store.Store) {
	t.Helper()
	_, storage := wakeService(t, &source.wakeSource)
	ctx := context.Background()
	queries, queryErr := storage.QuerySettings(ctx)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	queries.Config.Delay = policy.Delay{}
	if _, queryErr = storage.SaveQuerySettings(ctx, queries.Revision, queries.Config); queryErr != nil {
		t.Fatal(queryErr)
	}
	settings, err := storage.WakeSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.AllowedHours = []string{}
	settings.Config.Delay = policy.Delay{}
	if _, err = storage.SaveWakeSettings(ctx, settings.Revision, settings.Config); err != nil {
		t.Fatal(err)
	}
	source.catalog = []store.Account{{ID: "wake-account", AuthIndex: "wake-index", Binding: "private-binding", Status: "available"}}
	if _, err = storage.ApplyEvents(ctx, events.Batch{Instance: "wake-bridge", HostProcess: "wake-host", Earliest: 1, Events: []events.Event{}}); err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return service, storage
}

func awaitWake(t *testing.T, storage *store.Store, id, phase string) store.WakeJob {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		job, err := storage.Wake(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Phase == phase {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := storage.Wake(context.Background(), id)
	t.Fatalf("wanted %s got %+v", phase, job)
	return job
}

func TestWakeWorkerRunsWithoutPageAndVerifiesWithoutResending(t *testing.T) {
	source := &workerWakeSource{activateAfterSend: true}
	source.execute = func(_ context.Context, account store.Account, model, prompt string) error {
		if account.ID != "wake-account" || account.AuthIndex != "wake-index" || account.Binding != "private-binding" || account.Provider != "codex" || model != "private-model" || prompt != "private prompt content" {
			t.Errorf("wrong wake snapshot: %+v %q %q", account, model, prompt)
		}
		return nil
	}
	service, storage := prepareWakeWorker(t, source)
	initial := acceptWake(t, service, "manual-background")
	if source.wakeCalls.Load() != 0 {
		t.Fatal("POST executed model synchronously")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	finished := awaitWake(t, storage, initial.ID, "completed")
	if finished.ErrorCode != "window_verified" || source.wakeCalls.Load() != 1 || source.queryCalls.Load() < 2 {
		t.Fatal(finished, source.wakeCalls.Load(), source.queryCalls.Load())
	}
	duplicate := acceptWake(t, service, "manual-background")
	if duplicate.ID != initial.ID || duplicate.Phase != "completed" {
		t.Fatal(duplicate)
	}
	time.Sleep(250 * time.Millisecond)
	if source.wakeCalls.Load() != 1 {
		t.Fatal("completed wake was resent")
	}
}

func TestWakeWorkerPersistsPrecisePreflightFailures(t *testing.T) {
	for _, code := range []string{"wake_account_not_found", "wake_account_disabled", "wake_host_unavailable", "wake_inventory_stale", "wake_account_ambiguous", "wake_binding_missing", "wake_binding_changed", "wake_account_unavailable"} {
		t.Run(code, func(t *testing.T) {
			source := &workerWakeSource{}
			source.execute = func(context.Context, store.Account, string, string) error { return errors.New(code) }
			service, storage := prepareWakeWorker(t, source)
			initial := acceptWake(t, service, "worker-preflight")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); service.Run(ctx) }()
			defer func() { cancel(); <-done }()
			finished := awaitWake(t, storage, initial.ID, "failed")
			if finished.ErrorCode != code || finished.Response == nil || finished.Response.ErrorCode != code || source.wakeCalls.Load() != 1 {
				t.Fatal("worker swallowed precise classification", finished, source.wakeCalls.Load())
			}
			response := request(service, "GET", "/api/v2/wake-jobs?job_id="+initial.ID, "", "")
			if response.Code != 200 || !strings.Contains(response.Body.String(), code) || strings.Contains(response.Body.String(), "private-binding") || strings.Contains(response.Body.String(), "private-prompt") {
				t.Fatal("unsafe or imprecise worker result", response.Code, response.Body.String())
			}
		})
	}
}

func TestWakeWorkerCancellationPersistsUnknownAndRestartDoesNotResend(t *testing.T) {
	started := make(chan struct{})
	source := &workerWakeSource{}
	source.execute = func(ctx context.Context, _ store.Account, _, _ string) error {
		close(started)
		<-ctx.Done()
		return errors.New("private transport failure")
	}
	service, storage := prepareWakeWorker(t, source)
	initial := acceptWake(t, service, "cancelled-wake")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		cancel()
		<-done
		t.Fatal("wake worker did not dispatch")
	}
	cancel()
	<-done
	unknown := awaitWake(t, storage, initial.ID, "unknown")
	if unknown.ErrorCode != "wake_outcome_unknown" {
		t.Fatal(unknown)
	}
	restarted, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	source.unavailable.Store(true)
	duplicate := acceptWake(t, restarted, "cancelled-wake")
	if duplicate.ID != initial.ID || duplicate.Phase != "unknown" || duplicate.InstanceID == initial.InstanceID {
		t.Fatal(duplicate)
	}
	response := request(restarted, "GET", "/api/v2/wake-jobs?job_id="+initial.ID, "", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "private") || source.wakeCalls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); restarted.Run(ctx) }()
	time.Sleep(250 * time.Millisecond)
	cancel()
	<-done
	if source.wakeCalls.Load() != 1 {
		t.Fatal("unknown wake was resent after restart")
	}
}
