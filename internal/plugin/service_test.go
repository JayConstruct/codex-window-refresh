package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
)

type fakeCallback struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeCallback) Call(context.Context, string, any, any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errors.New("unexpected real host callback")
}

type fakeHost struct{}

func (fakeHost) Capabilities(context.Context) (core.HostCapabilities, error) {
	return core.HostCapabilities{}, nil
}
func (fakeHost) Accounts(context.Context) ([]core.HostAccount, error) { return nil, nil }
func (fakeHost) ObserveQuota(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
	return core.QuotaObservation{}, nil
}
func (fakeHost) SendExact(context.Context, core.ExactRequest) (core.SendResult, error) {
	return core.SendResult{}, errors.New("unexpected send")
}
func (fakeHost) ActivityAccounts() map[core.AccountID]core.HostAccount {
	return map[core.AccountID]core.HostAccount{"a": {ID: "a", Generation: "g", Enabled: true}}
}

type fakeStore struct {
	mu     sync.Mutex
	closed bool
}

func (*fakeStore) Load(context.Context) (core.CoreState, error)                   { return core.CoreState{}, nil }
func (*fakeStore) Commit(context.Context, uint64, core.CoreState) (uint64, error) { return 1, nil }
func (f *fakeStore) Close() error                                                 { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true; return nil }

type fakeEngine struct {
	mu           sync.Mutex
	events       []core.ActivityEvent
	observations []core.QuotaObservation
	updated      config.Config
	notified     int
	block        chan struct{}
}

func (f *fakeEngine) Run(ctx context.Context) error {
	if f.block != nil {
		<-f.block
	}
	<-ctx.Done()
	return nil
}
func (f *fakeEngine) Snapshot() core.CoreState {
	return core.CoreState{Revision: 42, Accounts: map[core.AccountID]core.AccountState{"a": {ID: "a", Generation: "g"}}}
}
func (f *fakeEngine) UpdateConfig(c config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = c
	return nil
}
func (f *fakeEngine) EnqueueActivity(e core.ActivityEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}
func (f *fakeEngine) EnqueueObservation(o core.QuotaObservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observations = append(f.observations, o)
}
func (f *fakeEngine) Notify() { f.mu.Lock(); defer f.mu.Unlock(); f.notified++ }
func (f *fakeEngine) Diagnostics() (core.HostCapabilities, string, int) {
	return core.HostCapabilities{ExactAccountExecution: true}, "", 1
}
func lifecyclePayload(t *testing.T, wrapper string, schema int) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"config_yaml": []byte(wrapper), "schema_version": schema})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func rpcResult(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("RPC failed: %s", raw)
	}
	return envelope.Result
}
func setupService(t *testing.T) (*Service, *fakeEngine, *fakeStore, *fakeCallback) {
	t.Helper()
	engine := &fakeEngine{}
	store := &fakeStore{}
	callback := &fakeCallback{}
	cfg := config.Defaults()
	cfg.Execution.Model = "gpt-5.4"
	s := New(callback, Options{EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
		return engine, nil
	}, HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil }, OpenStore: func(string) (DurableStore, error) { return store, nil }, LoadConfig: func(string) (config.Config, error) { return cfg, nil }, ShutdownTimeout: 20 * time.Millisecond})
	rpcResult(t, s.Handle(context.Background(), "plugin.register", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\n", 6)))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, engine, store, callback
}
func TestLifecycleRegistrationAndWrapperIsolation(t *testing.T) {
	s, _, store, cb := setupService(t)
	registrationRaw := rpcResult(t, s.Handle(context.Background(), "plugin.reconfigure", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\n", 6)))
	var r struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct{ Name, Version, Author, GitHubRepository string }
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if json.Unmarshal(registrationRaw, &r) != nil || r.SchemaVersion != 6 || r.Metadata.Name != ID || !r.Capabilities["request_lifecycle_plugin"] {
		t.Fatal("registration schema mismatched")
	}
	for _, capability := range []string{"response_interceptor", "response_stream_interceptor", "websocket_response_observer"} {
		if !r.Capabilities[capability] {
			t.Fatalf("missing quota observation capability: %s", capability)
		}
	}
	// CPA validPlugin rejects registrations missing any of these metadata fields.
	if r.Metadata.Version != Version || r.Metadata.Author == "" || r.Metadata.GitHubRepository != Repository {
		t.Fatal("metadata required by the CPA loader is incomplete")
	}
	for _, wrapper := range []string{"config_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\nreset_rules: {}\n", "config_file: relative.yaml\nstate_dir: /tmp/scheduler-state\n", "config_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\nmanagement_key: SECRET\n", "config_file: /tmp/policy.yaml\nstate_dir: /tmp/changed-state\n"} {
		raw := s.Handle(context.Background(), "plugin.reconfigure", lifecyclePayload(t, wrapper, 6))
		var env rpcEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.OK {
			t.Fatalf("invalid/mixed wrapper accepted: %s", wrapper)
		}
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	closed := store.closed
	store.mu.Unlock()
	if !closed {
		t.Fatal("store not closed after drain")
	}
	if cb.calls != 0 {
		t.Fatal("mock test made host callbacks")
	}
}
func TestLifecycleObservesWithoutChangingRequest(t *testing.T) {
	s, e, _, _ := setupService(t)
	body := []byte(`{"model":"x","input":"keep <this> unchanged"}`)
	raw, _ := json.Marshal(map[string]any{"RequestID": "r", "Body": body, "Headers": map[string][]string{"Authorization": {"unchanged"}, "x-repeat": {"one", "two"}}, "Metadata": map[string]any{"caller_scope": "trusted", "selected_auth_id": "a"}})
	for _, method := range []string{"request.intercept_before", "request.intercept_after"} {
		result := rpcResult(t, s.Handle(context.Background(), method, raw))
		var response struct {
			Headers   map[string][]string
			Body      []byte
			Terminate bool
		}
		if json.Unmarshal(result, &response) != nil || string(response.Body) != string(body) || len(response.Headers["x-repeat"]) != 2 || response.Headers["Authorization"][0] != "unchanged" || response.Terminate {
			t.Fatal("interceptor modified request")
		}
	}
	rpcResult(t, s.Handle(context.Background(), "request.complete", []byte(`{"RequestID":"r"}`)))
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) != 3 || e.events[0].Phase != core.ActivityStarted || e.events[1].AccountID != "a" || e.events[2].Phase != core.ActivityCompleted {
		t.Fatalf("activity callbacks misparsed: %+v", e.events)
	}
}
func managementCall(t *testing.T, s *Service, method, path string) managementResponse {
	t.Helper()
	payload, _ := json.Marshal(managementRequest{Method: method, Path: path})
	result := rpcResult(t, s.Handle(context.Background(), "management.handle", payload))
	var response managementResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}
func TestManagementSeparatesPublicResourceFromAuthenticatedData(t *testing.T) {
	s, e, _, _ := setupService(t)
	result := rpcResult(t, s.Handle(context.Background(), "management.register", nil))
	var routes struct {
		Routes    []struct{ Method, Path string }
		Resources []struct{ Path string }
	}
	_ = json.Unmarshal(result, &routes)
	if len(routes.Routes) != 8 || len(routes.Resources) != 3 {
		t.Fatal("management registration incomplete")
	}
	for _, route := range routes.Routes {
		if route.Path[:len(ManagementPrefix)] != ManagementPrefix {
			t.Fatal("route outside authenticated namespace")
		}
	}
	public := managementCall(t, s, "GET", ResourcePrefix+"/status")
	if public.StatusCode != 200 || string(public.Body) != statusHTML {
		t.Fatal("public page contains runtime data")
	}
	response := managementCall(t, s, "GET", ManagementPrefix+"/accounts")
	if response.StatusCode != 200 {
		t.Fatal("accounts API missing")
	}
	if response := managementCall(t, s, "GET", ResourcePrefix+"/accounts"); response.StatusCode != 404 {
		t.Fatal("public resource exposed accounts")
	}
	if response := managementCall(t, s, "GET", ManagementPrefix+"/recheck"); response.StatusCode != 404 {
		t.Fatal("unsafe route accepts GET")
	}
	if response := managementCall(t, s, "POST", ManagementPrefix+"/recheck"); response.StatusCode != 202 {
		t.Fatal("recheck not queued")
	}
	e.mu.Lock()
	notified := e.notified
	e.mu.Unlock()
	if notified != 1 {
		t.Fatal("recheck did not notify scheduler")
	}
	if response := managementCall(t, s, "POST", ManagementPrefix+"/reload"); response.StatusCode != 200 {
		t.Fatal("reload failed")
	}
}

type schedulerDiagnosticEngine struct{ *fakeEngine }

func (f *schedulerDiagnosticEngine) Snapshot() core.CoreState {
	state := f.fakeEngine.Snapshot()
	state.Activity = map[core.UserIdentity]core.UserActivity{
		"private-caller-identity": {Identity: "private-caller-identity", InFlightCount: 1},
	}
	state.Requests = map[core.RequestID]core.RequestActivity{
		"private-request-id": {Identity: "private-caller-identity", AccountID: "a", Generation: "g"},
	}
	return state
}
func (*schedulerDiagnosticEngine) SchedulerDiagnostics() any {
	return map[string]any{
		"observed_at":              time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
		"recent_users":             2,
		"identity_source":          "host_caller_scope",
		"capacity":                 map[string]int{"target": 2, "active": 1, "reserved": 0, "missing": 1},
		"hold_counts":              map[string]int{"active_window": 1},
		"quota_unknown_accounts":   0,
		"foreground_busy_accounts": 1,
	}
}

func TestSummaryPublishesOptionalAggregateSchedulerDiagnostics(t *testing.T) {
	s, engine, _, _ := setupService(t)
	// Existing engines implementing only the original interface keep the same
	// management schema without a misleading fabricated scheduler report.
	before := managementCall(t, s, "GET", ManagementPrefix+"/summary")
	var original map[string]json.RawMessage
	if err := json.Unmarshal(before.Body, &original); err != nil {
		t.Fatal(err)
	}
	if _, ok := original["scheduler"]; ok {
		t.Fatal("optional diagnostics appeared for an engine without the interface")
	}
	s.mu.Lock()
	s.engine = &schedulerDiagnosticEngine{fakeEngine: engine}
	s.mu.Unlock()
	response := managementCall(t, s, "GET", ManagementPrefix+"/summary")
	var summary struct {
		Revision  uint64 `json:"revision"`
		Scheduler struct {
			RecentUsers    int            `json:"recent_users"`
			IdentitySource string         `json:"identity_source"`
			Capacity       map[string]int `json:"capacity"`
			HoldCounts     map[string]int `json:"hold_counts"`
		} `json:"scheduler"`
	}
	if err := json.Unmarshal(response.Body, &summary); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || summary.Revision != 42 || summary.Scheduler.RecentUsers != 2 || summary.Scheduler.IdentitySource != "host_caller_scope" || summary.Scheduler.Capacity["missing"] != 1 || summary.Scheduler.HoldCounts["active_window"] != 1 {
		t.Fatalf("scheduler aggregates missing: %s", response.Body)
	}
	for _, private := range []string{"private-caller-identity", "private-request-id", "\"Activity\"", "\"Requests\""} {
		if strings.Contains(string(response.Body), private) {
			t.Fatalf("summary exposed private activity: %s", response.Body)
		}
		public := managementCall(t, s, "GET", ResourcePrefix+"/status")
		if strings.Contains(string(public.Body), private) || string(public.Body) != statusHTML {
			t.Fatal("public resource changed to runtime data")
		}
	}
}
func TestShutdownRetainsStoreUntilCallbacksDrain(t *testing.T) {
	e := &fakeEngine{block: make(chan struct{})}
	store := &fakeStore{}
	cfg := config.Defaults()
	cfg.Execution.Model = "gpt-5.4"
	s := New(&fakeCallback{}, Options{EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) { return e, nil }, HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil }, OpenStore: func(string) (DurableStore, error) { return store, nil }, LoadConfig: func(string) (config.Config, error) { return cfg, nil }, ShutdownTimeout: time.Millisecond})
	rpcResult(t, s.Handle(context.Background(), "plugin.register", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/blocked-state\n", 6)))
	if err := s.Shutdown(context.Background()); err == nil {
		t.Fatal("blocked callback shutdown reported success")
	}
	store.mu.Lock()
	closed := store.closed
	store.mu.Unlock()
	if closed {
		t.Fatal("store closed while engine still using it")
	}
	close(e.block)
	s.WaitForShutdown()
	store.mu.Lock()
	closed = store.closed
	store.mu.Unlock()
	if !closed {
		t.Fatal("drained store left open")
	}
}

func TestUnsupportedIdentityReloadPreservesPolicy(t *testing.T) {
	s, e, _, _ := setupService(t)
	unsupported := config.Defaults()
	unsupported.Execution.Model = "gpt-5.4"
	unsupported.Activity.IdentitySource = "api_key_hash"
	s.options.LoadConfig = func(string) (config.Config, error) { return unsupported, nil }
	if result := managementCall(t, s, "POST", ManagementPrefix+"/reload"); result.StatusCode != 400 {
		t.Fatal("CPA adapter accepted unsupported identity mode")
	}
	s.mu.RLock()
	source := s.cfg.Activity.IdentitySource
	s.mu.RUnlock()
	if source != "host_caller_scope" {
		t.Fatal("invalid reload changed policy")
	}
	e.mu.Lock()
	source = e.updated.Activity.IdentitySource
	e.mu.Unlock()
	if source != "" {
		t.Fatal("invalid mode was sent to engine")
	}
}
func TestFactoryPanicIsContainedAndStoreClosed(t *testing.T) {
	store := &fakeStore{}
	cfg := config.Defaults()
	cfg.Execution.Model = "gpt-5.4"
	s := New(&fakeCallback{}, Options{EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
		panic("sensitive detail")
	}, HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil }, OpenStore: func(string) (DurableStore, error) { return store, nil }, LoadConfig: func(string) (config.Config, error) { return cfg, nil }})
	result := s.Handle(context.Background(), "plugin.register", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/panic-state\n", 6))
	var env struct {
		OK    bool      `json:"ok"`
		Error *rpcError `json:"error"`
	}
	_ = json.Unmarshal(result, &env)
	if env.OK || env.Error == nil || env.Error.Code != "plugin_panic" || env.Error.Message != "plugin operation failed" {
		t.Fatalf("panic leaked: %s", result)
	}
	store.mu.Lock()
	closed := store.closed
	store.mu.Unlock()
	if !closed {
		t.Fatal("factory panic leaked state lock")
	}
}

type backgroundPanicEngine struct{ fakeEngine }

func (*backgroundPanicEngine) Run(context.Context) error { panic("untrusted host panic detail") }
func TestBackgroundPanicIsContainedAndRetainsStoreUntilShutdown(t *testing.T) {
	engine := &backgroundPanicEngine{}
	store := &fakeStore{}
	cfg := config.Defaults()
	cfg.Execution.Model = "gpt-5.4"
	s := New(&fakeCallback{}, Options{EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
		return engine, nil
	}, HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil }, OpenStore: func(string) (DurableStore, error) { return store, nil }, LoadConfig: func(string) (config.Config, error) { return cfg, nil }, ShutdownTimeout: time.Second})
	rpcResult(t, s.Handle(context.Background(), "plugin.register", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/background-panic-state\n", 6)))
	s.mu.RLock()
	done := s.done
	s.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background panic did not complete lifecycle")
	}
	s.mu.RLock()
	lastError := s.lastError
	s.mu.RUnlock()
	if lastError != "scheduler stopped due to a panic" {
		t.Fatalf("background panic not sanitized: %s", lastError)
	}
	store.mu.Lock()
	closed := store.closed
	store.mu.Unlock()
	if closed {
		t.Fatal("state lock closed outside explicit shutdown")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	closed = store.closed
	store.mu.Unlock()
	if !closed {
		t.Fatal("completed failed runtime did not release state lock on shutdown")
	}
}

func TestHostInjectedControlsAreTypedAndIndependentOfBusinessPolicy(t *testing.T) {
	s, e, _, _ := setupService(t)
	payload := func(enabled, priority string) []byte {
		return lifecyclePayload(t, "enabled: "+enabled+"\npriority: "+priority+"\nconfig_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\n", 6)
	}
	// Both fields are injected by CPA even when omitted from user YAML. Changing
	// priority is host metadata, not a plumbing migration requiring a restart.
	rpcResult(t, s.Handle(context.Background(), "plugin.reconfigure", payload("true", "7")))
	e.mu.Lock()
	enabled := e.updated.Enabled
	e.mu.Unlock()
	if !enabled {
		t.Fatal("host true incorrectly disabled enabled business policy")
	}
	rpcResult(t, s.Handle(context.Background(), "plugin.reconfigure", payload("false", "-2")))
	e.mu.Lock()
	enabled = e.updated.Enabled
	e.mu.Unlock()
	if enabled {
		t.Fatal("host false did not safely gate scheduler")
	}
	s.mu.RLock()
	businessEnabled := s.cfg.Enabled
	s.mu.RUnlock()
	if !businessEnabled {
		t.Fatal("host control overwrote persisted business policy")
	}
	// Re-loading a true business file while the host gate is false stays disabled.
	if response := managementCall(t, s, "POST", ManagementPrefix+"/reload"); response.StatusCode != 200 {
		t.Fatal("reload failed")
	}
	e.mu.Lock()
	enabled = e.updated.Enabled
	e.mu.Unlock()
	if enabled {
		t.Fatal("reload bypassed host false gate")
	}
	disabled := config.Defaults()
	disabled.Enabled = false
	disabled.Execution.Model = "gpt-5.4"
	s.options.LoadConfig = func(string) (config.Config, error) { return disabled, nil }
	rpcResult(t, s.Handle(context.Background(), "plugin.reconfigure", payload("true", "0")))
	e.mu.Lock()
	enabled = e.updated.Enabled
	e.mu.Unlock()
	if enabled {
		t.Fatal("host true overrode business false")
	}
	for _, p := range [][]byte{payload("\"true\"", "0"), payload("true", "\"1\""), payload("true", "1.0"), payload("null", "0"), payload("true", "null")} {
		var env struct {
			OK bool `json:"ok"`
		}
		raw := s.Handle(context.Background(), "plugin.reconfigure", p)
		_ = json.Unmarshal(raw, &env)
		if env.OK {
			t.Fatalf("host-owned control coercion accepted: %s", p)
		}
	}
}
