// Package plugin implements the CPA JSON-RPC lifecycle and authenticated
// management surface. It owns no C pointers; the native entry is in cmd/plugin.
package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	scheduler "codex-window-refresh/internal/runtime"
	"codex-window-refresh/internal/storage"
	"gopkg.in/yaml.v3"
)

const ID = "codex-window-refresh"
const Version = "0.1.0-rc.27"
const Repository = "https://github.com/JayConstruct/codex-window-refresh"
const SchemaVersion = 6
const ManagementPrefix = "/v0/management/" + ID
const ResourcePrefix = "/v0/resource/plugins/" + ID

type quotaBridgeDiagnostic struct {
	Mode             string `json:"mode"`
	Status           string `json:"status"`
	ManagementKeyEnv string `json:"management_key_env,omitempty"`
	HostHTTPStatus   string `json:"host_http_status,omitempty"`
}

type Engine interface {
	Run(context.Context) error
	Snapshot() core.CoreState
	// UpdateConfig must leave the previous configuration intact on error.
	UpdateConfig(config.Config) error
	EnqueueActivity(core.ActivityEvent)
	EnqueueObservation(core.QuotaObservation)
	Notify()
}
type DiagnosticEngine interface {
	Diagnostics() (core.HostCapabilities, string, int)
}

// SchedulerDiagnosticEngine provides aggregate scheduling decisions only. The
// optional interface keeps other Engine implementations compatible; values must
// not contain activity identities, request metadata, or credentials.
type SchedulerDiagnosticEngine interface {
	SchedulerDiagnostics() any
}
type Host interface {
	core.Host
	ActivityAccounts() map[core.AccountID]core.HostAccount
}
type DurableStore interface {
	core.Store
	Close() error
}
type Options struct {
	LibraryPath     string
	EngineFactory   func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error)
	HostFactory     func(cpa.Callback, WrapperConfig) (Host, error)
	OpenStore       func(string) (DurableStore, error)
	LoadConfig      func(string) (config.Config, error)
	ShutdownTimeout time.Duration
}

// WrapperConfig contains plumbing only. All scheduling policy is read from the
// separate validated business config; host credentials remain memory-only.
type WrapperConfig struct {
	// CPA injects these host-owned loader controls into ConfigYAML. They do not
	// belong to business policy and do not change plumbing identity.
	HostEnabled      *bool     `yaml:"enabled" json:"-"`
	HostPriority     *int      `yaml:"priority" json:"-"`
	HostStore        yaml.Node `yaml:"store" json:"-"`
	ConfigFile       string    `yaml:"config_file" json:"config_file"`
	StateDir         string    `yaml:"state_dir" json:"state_dir"`
	QuotaTransport   string    `yaml:"quota_transport" json:"quota_transport,omitempty"`
	ManagementURL    string    `yaml:"management_url" json:"management_url,omitempty"`
	ManagementKeyEnv string    `yaml:"management_key_env" json:"management_key_env,omitempty"`
}

func (w WrapperConfig) quotaTransport() string {
	if w.QuotaTransport == "" {
		return "host"
	}
	return w.QuotaTransport
}

func (w WrapperConfig) hostEnabled() bool { return w.HostEnabled == nil || *w.HostEnabled }
func (w WrapperConfig) effective(cfg config.Config) config.Config {
	businessEnabled := cfg.Enabled
	cfg.BusinessEnabled = &businessEnabled
	cfg.Enabled = cfg.Enabled && w.hostEnabled()
	cfg.ManualWakeDisabled = !w.hostEnabled()
	return cfg
}
func (w WrapperConfig) samePlumbing(other WrapperConfig) bool {
	return w.ConfigFile == other.ConfigFile && w.StateDir == other.StateDir && w.ManagementURL == other.ManagementURL && w.ManagementKeyEnv == other.ManagementKeyEnv && w.quotaTransport() == other.quotaTransport()
}

type Service struct {
	versionMu     sync.Mutex
	instanceID    string
	calls         sync.RWMutex
	lifecycle     sync.Mutex
	mu            sync.RWMutex
	callback      cpa.Callback
	options       Options
	wrapper       WrapperConfig
	cfg           config.Config
	engine        Engine
	host          Host
	store         DurableStore
	cancel        context.CancelFunc
	done          chan struct{}
	stopping      bool
	quiesced      bool
	lastError     string
	policyOps     *policyFileOps
	responseQuota cpa.ResponseQuotaObserver
}

func New(callback cpa.Callback, options Options) *Service {
	if options.OpenStore == nil {
		options.OpenStore = func(path string) (DurableStore, error) { return storage.Open(path) }
	}
	if options.LoadConfig == nil {
		options.LoadConfig = config.Load
	}
	if options.HostFactory == nil {
		options.HostFactory = func(cb cpa.Callback, w WrapperConfig) (Host, error) {
			if w.quotaTransport() == "host" {
				return cpa.NewHost(cb, &cpa.NativeReader{Callback: cb}), nil
			}
			return cpa.NewHost(cb, &cpa.BridgeReader{ManagementURL: w.ManagementURL, ManagementKeyEnv: w.ManagementKeyEnv, Getenv: os.Getenv}), nil
		}
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 5 * time.Second
	}
	return &Service{callback: callback, options: options}
}

type rpcEnvelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}
type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ErrorResponse(code, message string) []byte {
	raw, _ := json.Marshal(rpcEnvelope{Error: &rpcError{Code: code, Message: message}})
	return raw
}
func (s *Service) Handle(ctx context.Context, method string, payload []byte) (raw []byte) {
	defer func() {
		if recover() != nil {
			raw = ErrorResponse("plugin_panic", "plugin operation failed")
		}
	}()
	result, err := s.handle(ctx, method, payload)
	if err != nil {
		return ErrorResponse("plugin_error", err.Error())
	}
	raw, err = json.Marshal(rpcEnvelope{OK: true, Result: result})
	if err != nil {
		return ErrorResponse("encoding_error", "response encoding failed")
	}
	return raw
}
func (s *Service) handle(ctx context.Context, method string, payload []byte) (any, error) {
	// Lifecycle writers drain management operations and observational hooks before
	// releasing a host or store. Acquire this gate before the lifecycle mutex.
	if method != "plugin.register" && method != "plugin.reconfigure" && method != "plugin.quiesce" && method != "plugin.shutdown" {
		s.calls.RLock()
		defer s.calls.RUnlock()
	}
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := s.configureContext(ctx, payload); err != nil {
			return nil, err
		}
		return registration(), nil
	case "plugin.quiesce":
		if err := s.Quiesce(ctx); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case "plugin.shutdown":
		if err := s.Shutdown(ctx); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return s.management(ctx, payload)
	case "request.intercept_before", "request.intercept_after", "request.complete":
		// Observational hooks fail open for invalid optional activity metadata.
		pass, err := cpa.PassThrough(method, payload)
		if err != nil {
			return nil, errors.New("invalid request lifecycle payload")
		}
		s.mu.RLock()
		engine, host, stopping := s.engine, s.host, s.stopping
		s.mu.RUnlock()
		if engine != nil && host != nil && !stopping {
			accounts := host.ActivityAccounts()
			s.responseQuota.Track(method, payload, accounts, time.Now().UTC())
			events, err := cpa.ParseActivity(method, payload, accounts)
			if err == nil {
				for _, event := range events {
					engine.EnqueueActivity(event)
				}
			}
		}
		return pass, nil
	case "response.intercept_after", "response.intercept_stream_chunk", "websocket.response_event":
		// An empty response is the CPA contract for leaving every header, byte,
		// and stream chunk untouched. Malformed optional quota fails open.
		s.mu.RLock()
		engine, host, stopping := s.engine, s.host, s.stopping
		s.mu.RUnlock()
		if engine != nil && host != nil && !stopping {
			for _, observation := range s.responseQuota.Observe(method, payload, host.ActivityAccounts, time.Now().UTC()) {
				engine.EnqueueObservation(observation)
			}
		}
		return struct{}{}, nil
	default:
		return nil, errors.New("unknown method")
	}
}
func parseWrapper(payload []byte) (WrapperConfig, error) {
	var lifecycle struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if err := json.Unmarshal(payload, &lifecycle); err != nil {
		return WrapperConfig{}, errors.New("invalid lifecycle request")
	}
	if lifecycle.SchemaVersion < SchemaVersion {
		return WrapperConfig{}, fmt.Errorf("host schema version must be at least %d", SchemaVersion)
	}
	dec := yaml.NewDecoder(bytes.NewReader(lifecycle.ConfigYAML))
	dec.KnownFields(true)
	var wrapper WrapperConfig
	if err := dec.Decode(&wrapper); err != nil {
		return WrapperConfig{}, errors.New("invalid wrapper configuration")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return WrapperConfig{}, errors.New("wrapper configuration must contain one document")
	}
	// YAML decoding must not coerce quoted host booleans/integers or nulls.
	var document yaml.Node
	if err := yaml.Unmarshal(lifecycle.ConfigYAML, &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return WrapperConfig{}, errors.New("invalid wrapper configuration")
	}
	mapping := document.Content[0]
	for i := 0; i < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Value == "enabled" && (value.Kind != yaml.ScalarNode || value.Tag != "!!bool") {
			return WrapperConfig{}, errors.New("host enabled must be a boolean")
		}
		if key.Value == "priority" && (value.Kind != yaml.ScalarNode || value.Tag != "!!int") {
			return WrapperConfig{}, errors.New("host priority must be an integer")
		}
	}
	if wrapper.ConfigFile == "" || wrapper.StateDir == "" || !filepath.IsAbs(wrapper.ConfigFile) || !filepath.IsAbs(wrapper.StateDir) {
		return WrapperConfig{}, errors.New("config_file and state_dir must be absolute paths")
	}
	if (wrapper.ManagementURL == "") != (wrapper.ManagementKeyEnv == "") {
		return WrapperConfig{}, errors.New("management_url and management_key_env must be set together")
	}
	if wrapper.quotaTransport() != "host" && wrapper.quotaTransport() != "management" {
		return WrapperConfig{}, errors.New("quota_transport must be host or management")
	}
	if wrapper.quotaTransport() == "management" && wrapper.ManagementURL == "" {
		return WrapperConfig{}, errors.New("management transport requires management_url and management_key_env")
	}
	return wrapper, nil
}
func (s *Service) configure(payload []byte) error {
	return s.configureContext(context.Background(), payload)
}
func (s *Service) configureContext(lifecycleContext context.Context, payload []byte) error {
	wrapper, err := parseWrapper(payload)
	if err != nil {
		return err
	}
	s.calls.Lock()
	defer s.calls.Unlock()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	cfg, err := s.options.LoadConfig(wrapper.ConfigFile)
	if err != nil {
		return errors.New("business configuration failed to load or validate")
	}
	if err := cfg.Validate(); err != nil {
		return errors.New("business configuration failed to validate")
	}
	if cfg.Activity.IdentitySource != "host_caller_scope" {
		return errors.New("identity_source_not_supported_by_cpa_adapter")
	}
	s.mu.RLock()
	existing, previous, stopping, quiesced, previousDone := s.engine, s.wrapper, s.stopping, s.quiesced, s.done
	s.mu.RUnlock()
	if stopping && !quiesced {
		return errors.New("plugin is stopping; process restart required")
	}
	if existing != nil && !wrapper.samePlumbing(previous) {
		return errors.New("wrapper plumbing changes require process restart")
	}
	if quiesced {
		// Rollback may arrive after a timed-out quiesce. Drain before reopening
		// the same state directory, and refuse a second writer while still busy.
		if err := s.stopRuntime(lifecycleContext); err != nil {
			return err
		}
		existing = nil
	}
	if existing != nil {
		if !schedulerRunning(previousDone, stopping) {
			return errors.New("scheduler stopped; cannot acknowledge reconfiguration")
		}
		if err := existing.UpdateConfig(wrapper.effective(cfg)); err != nil {
			return errors.New("configuration update failed")
		}
		s.mu.Lock()
		s.cfg = cfg
		s.wrapper = wrapper
		s.mu.Unlock()
		return s.publishRuntimeVersion("running", nil)
	}
	if s.options.EngineFactory == nil {
		return errors.New("scheduler factory unavailable")
	}
	host, err := s.options.HostFactory(s.callback, wrapper)
	if err != nil {
		return errors.New("host adapter initialization failed")
	}
	hostCommitted := false
	defer func() {
		if !hostCommitted {
			closeHostConnections(host)
		}
	}()
	store, err := s.options.OpenStore(wrapper.StateDir)
	if err != nil {
		return errors.New("state store initialization failed")
	}
	committed := false
	defer func() {
		if !committed {
			_ = store.Close()
		}
	}()
	engine, err := s.options.EngineFactory(wrapper.effective(cfg), host, store, realClock{}, secureRandom{})
	if err != nil {
		return errors.New("scheduler initialization failed")
	}
	instanceID, err := newRuntimeInstance()
	if err != nil {
		return errors.New("runtime instance identity initialization failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	s.wrapper = wrapper
	s.cfg = cfg
	s.engine = engine
	s.instanceID = instanceID
	s.host = host
	s.store = store
	s.cancel = cancel
	s.done = done
	s.stopping, s.quiesced, s.lastError = false, false, ""
	s.responseQuota = cpa.ResponseQuotaObserver{}
	s.mu.Unlock()
	if err := s.publishRuntimeVersion("running", nil); err != nil {
		cancel()
		close(done)
		s.mu.Lock()
		s.engine, s.store, s.host, s.cancel, s.done = nil, nil, nil, nil, nil
		s.mu.Unlock()
		return err
	}
	committed = true
	hostCommitted = true
	go func() {
		defer close(done)
		defer func() { _ = s.publishRuntimeVersion("stopped", done) }()
		defer func() {
			if recover() != nil {
				s.mu.Lock()
				s.lastError = "scheduler stopped due to a panic"
				s.mu.Unlock()
			}
		}()
		err := engine.Run(ctx)
		s.mu.Lock()
		if err != nil && !errors.Is(err, context.Canceled) {
			s.lastError = "scheduler stopped due to an error"
		}
		s.mu.Unlock()
	}()
	return nil
}

// Shutdown is bounded. On timeout the store remains owned and new lifecycle
// activity is refused. Native shutdown must subsequently call WaitForShutdown
// before its host frees the callback context or unloads the dynamic library.
func (s *Service) Shutdown(ctx context.Context) error {
	s.calls.Lock()
	defer s.calls.Unlock()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	s.quiesced = false
	s.mu.Unlock()
	return s.stopRuntime(ctx)
}

// Quiesce releases exclusive resources but can be reversed by reconfigure if
// CPA cannot register the replacement library. Final shutdown is irreversible.
func (s *Service) Quiesce(ctx context.Context) error {
	s.calls.Lock()
	defer s.calls.Unlock()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.stopping && !s.quiesced {
		s.mu.Unlock()
		return errors.New("plugin is shut down")
	}
	s.quiesced = true
	s.mu.Unlock()
	return s.stopRuntime(ctx)
}

// Caller owns calls and lifecycle, so callbacks cannot outlive store ownership.
func (s *Service) stopRuntime(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	cancel, done, store, host, engine, quiesced := s.cancel, s.done, s.store, s.host, s.engine, s.quiesced
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := s.publishRuntimeVersion("quiescing", nil); err != nil {
		return err
	}
	if done != nil {
		timer := time.NewTimer(s.options.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("lifecycle interrupted; state remains locked")
		case <-timer.C:
			return errors.New("lifecycle waiting for host callback; state remains locked")
		}
	}
	if quiesced && store != nil {
		if preparer, ok := engine.(interface{ PrepareHotReload(context.Context) error }); ok {
			if err := preparer.PrepareHotReload(ctx); err != nil {
				return errors.New("hot reload state handoff failed")
			}
		}
	}
	closeHostConnections(host)
	phase := "stopped"
	if quiesced {
		phase = "quiesced"
	}
	if err := s.publishRuntimeVersion(phase, nil); err != nil {
		return err
	}
	if store != nil {
		if err := store.Close(); err != nil {
			return errors.New("state store close failed")
		}
	}
	s.mu.Lock()
	s.store = nil
	s.cancel = nil
	s.done = nil
	s.host = nil
	s.mu.Unlock()
	return nil
}

// WaitForShutdown drains callbacks without a deadline. Native shutdown has no
// error return and cannot safely permit unloading when a C callback is still
// running. A nonreturning host callback therefore delays native shutdown.
func (s *Service) WaitForShutdown() {
	s.calls.Lock()
	defer s.calls.Unlock()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.RLock()
	done, store, host := s.done, s.store, s.host
	s.mu.RUnlock()
	if done != nil {
		<-done
	}
	closeHostConnections(host)
	_ = s.publishRuntimeVersion("stopped", nil)
	if store != nil {
		_ = store.Close()
	}
	s.mu.Lock()
	s.store = nil
	s.cancel = nil
	s.done = nil
	s.host = nil
	s.mu.Unlock()
}

func closeHostConnections(host Host) {
	if closer, ok := host.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

func registration() any {
	fields := []map[string]string{}
	for _, field := range []struct{ name, description string }{{"config_file", "Absolute path to the validated scheduler YAML or JSON file."}, {"state_dir", "Absolute path to private durable state directory (0700)."}, {"quota_transport", "Quota query transport: host (default) or management (optional compatibility bridge)."}, {"management_url", "Local CPA management URL, required only with management transport."}, {"management_key_env", "Management key environment variable name, required only with management transport."}} {
		fields = append(fields, map[string]string{"Name": field.name, "Type": "string", "Description": field.description})
	}
	return map[string]any{"schema_version": SchemaVersion, "metadata": map[string]any{"Name": ID, "Version": Version, "Author": "codex-window-refresh contributors", "GitHubRepository": Repository, "ConfigFields": fields}, "capabilities": map[string]bool{"request_interceptor": true, "request_lifecycle_plugin": true, "response_interceptor": true, "response_stream_interceptor": true, "websocket_response_observer": true, "management_api": true}}
}
func managementRegistration() any {
	routes := []map[string]string{}
	for _, p := range []string{"/snapshot", "/summary", "/accounts", "/config", "/config-editor", "/capabilities", "/history", "/lifecycle"} {
		routes = append(routes, map[string]string{"Method": "GET", "Path": ManagementPrefix + p})
	}
	for _, p := range []string{"/reload", "/recheck", "/config", "/quota-query", "/quota-connection-check", "/manual-wake", "/prompt-generation", "/refresh-accounts"} {
		routes = append(routes, map[string]string{"Method": "POST", "Path": ManagementPrefix + p})
	}
	return map[string]any{"Routes": routes, "Resources": []map[string]string{{"Path": "/status", "Menu": "Codex Window Refresh", "Description": "Authenticated scheduler panel."}, {"Path": "/panel.js"}, {"Path": "/panel.css"}}}
}

type managementRequest struct {
	Method, Path string
	Headers      http.Header
	Body         []byte
	Query        url.Values
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func response(status int, value any) managementResponse {
	body, _ := json.Marshal(value)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: body}
}
func (s *Service) management(ctx context.Context, payload []byte) (any, error) {
	var req managementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, errors.New("invalid management payload")
	}
	// CPA resources are public static assets. All account and policy data stays
	// under CPA-authenticated management routes.
	if req.Method == "GET" {
		if resource, ok := panelResource(req.Path); ok {
			return resource, nil
		}
	}
	if !strings.HasPrefix(req.Path, ManagementPrefix+"/") {
		return response(404, map[string]string{"error": "not found"}), nil
	}
	path := strings.TrimPrefix(req.Path, ManagementPrefix)
	if strings.HasPrefix(path, "/history?") {
		query, err := url.ParseQuery(strings.TrimPrefix(path, "/history?"))
		if err != nil {
			return response(400, map[string]string{"error": "invalid_history_filter"}), nil
		}
		if len(req.Query) > 0 {
			return response(400, map[string]string{"error": "invalid_history_filter"}), nil
		}
		req.Query, path = query, "/history"
	}
	s.mu.RLock()
	engine, wrapper, cfg, done, stopping, lastError := s.engine, s.wrapper, s.cfg, s.done, s.stopping, s.lastError
	s.mu.RUnlock()
	if req.Method == "GET" && path == "/lifecycle" {
		s.mu.RLock()
		quiesced := s.quiesced
		s.mu.RUnlock()
		phase := "stopped"
		if quiesced {
			phase = "quiesced"
		} else if schedulerRunning(done, stopping) {
			phase = "running"
		}
		instance := ""
		if reader, ok := engine.(interface {
			ManagementSnapshot() scheduler.ManagementSnapshot
		}); ok {
			instance = reader.ManagementSnapshot().InstanceID
		}
		return response(200, map[string]any{"version": Version, "hot_reload_protocol": HotReloadProtocol, "phase": phase, "instance_id": instance}), nil
	}
	if engine == nil {
		return response(503, map[string]string{"error": "scheduler not initialized"}), nil
	}
	switch req.Method + " " + path {
	case "GET /snapshot":
		return s.snapshotResponse(engine, wrapper, done, stopping, lastError), nil
	case "POST /refresh-accounts":
		// Keep the host callback and store alive until this synchronous read and
		// publication finish, including when shutdown races the request.
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.RLock()
		engine, wrapper, done, stopping, lastError = s.engine, s.wrapper, s.done, s.stopping, s.lastError
		s.mu.RUnlock()
		if !schedulerRunning(done, stopping) {
			return response(409, map[string]string{"error": "scheduler stopping"}), nil
		}
		refresher, ok := engine.(interface{ RefreshAccounts(context.Context) error })
		if !ok {
			return response(501, map[string]string{"error": "account_refresh_unavailable"}), nil
		}
		if err := refresher.RefreshAccounts(ctx); err != nil {
			return response(503, map[string]string{"error": "account_refresh_failed"}), nil
		}
		return s.snapshotResponse(engine, wrapper, done, stopping, lastError), nil
	case "GET /summary":
		state := engine.Snapshot()
		running := done != nil && !stopping
		if done != nil {
			select {
			case <-done:
				running = false
			default:
			}
		}
		concurrency := 1
		if debug, ok := engine.(DiagnosticEngine); ok {
			_, errorCode, effective := debug.Diagnostics()
			if errorCode != "" {
				lastError = errorCode
			}
			concurrency = effective
		}
		summary := map[string]any{"enabled": wrapper.effective(cfg).Enabled, "business_enabled": cfg.Enabled, "host_enabled": wrapper.hostEnabled(), "running": running, "revision": state.Revision, "accounts": len(state.Accounts), "reservations": len(state.Reservations), "last_error": lastError, "effective_concurrency": concurrency}
		if debug, ok := engine.(SchedulerDiagnosticEngine); ok {
			summary["scheduler"] = debug.SchedulerDiagnostics()
		}
		return response(200, summary), nil
	case "GET /accounts":
		return response(200, engine.Snapshot().Accounts), nil
	case "GET /history":
		return historyResponse(engine, req.Query), nil
	case "POST /prompt-generation":
		return generationResponse(engine, wrapper, done, stopping, req.Body), nil
	case "GET /config":
		return response(200, cfg), nil
	case "GET /config-editor":
		return s.readPolicyEditor(), nil
	case "POST /config":
		return s.savePolicyEditor(ctx, req.Body), nil
	case "POST /manual-wake":
		return manualWakeResponse(engine, wrapper, cfg, done, stopping, req.Body), nil
	case "POST /quota-query":
		if stopping || done == nil {
			return response(409, map[string]string{"error": "scheduler_not_running"}), nil
		}
		select {
		case <-done:
			return response(409, map[string]string{"error": "scheduler_not_running"}), nil
		default:
		}
		// Only an optional exact account selector is accepted, never model input.
		var target core.AccountID
		if raw := bytes.TrimSpace(req.Body); len(raw) > 0 {
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) > 1 {
				return response(400, map[string]string{"error": "invalid_quota_query_parameters"}), nil
			}
			if len(fields) == 1 {
				value, ok := fields["account_id"]
				if !ok || json.Unmarshal(value, &target) != nil || strings.TrimSpace(string(target)) == "" {
					return response(400, map[string]string{"error": "invalid_quota_query_parameters"}), nil
				}
				if _, exists := engine.Snapshot().Accounts[target]; !exists {
					return response(404, map[string]string{"error": "account_not_found"}), nil
				}
			}
		}
		query, ok := engine.(interface{ RequestQuotaQuery() bool })
		if !ok {
			return response(501, map[string]string{"error": "quota_query_unavailable"}), nil
		}
		if debug, ok := engine.(DiagnosticEngine); ok {
			caps, _, _ := debug.Diagnostics()
			if !caps.QuotaAdapterReady {
				return response(503, map[string]string{"error": "quota_bridge_unavailable"}), nil
			}
		}
		status, jobID := "already_queued", ""
		if selected, ok := engine.(interface {
			QueueQuotaQueryJob(core.AccountID) (string, string)
		}); ok {
			status, jobID = selected.QueueQuotaQueryJob(target)
			if status != "queued" && status != "already_queued" {
				return response(409, map[string]string{"error": status}), nil
			}
		} else if selected, ok := engine.(interface{ QueueQuotaQuery(core.AccountID) string }); ok {
			status = selected.QueueQuotaQuery(target)
			if status == "busy" {
				return response(409, map[string]string{"error": "quota_query_busy"}), nil
			}
		} else if target != "" {
			return response(501, map[string]string{"error": "account_quota_query_unavailable"}), nil
		} else if query.RequestQuotaQuery() {
			status = "queued"
		}
		return response(202, map[string]any{"status": status, "account_id": target, "job_id": jobID}), nil
	case "GET /capabilities":
		capabilities := core.HostCapabilities{}
		if debug, ok := engine.(DiagnosticEngine); ok {
			capabilities, _, _ = debug.Diagnostics()
		}
		bridge := &cpa.BridgeReader{ManagementURL: wrapper.ManagementURL, ManagementKeyEnv: wrapper.ManagementKeyEnv}
		diagnostic := quotaBridgeDiagnostic{Mode: wrapper.quotaTransport()}
		if diagnostic.Mode == "host" {
			diagnostic.Status = "host_callback"
			s.mu.RLock()
			host := s.host
			s.mu.RUnlock()
			if status, ok := host.(interface{ QuotaConnectionStatus() string }); ok {
				diagnostic.HostHTTPStatus = status.QuotaConnectionStatus()
			}
		} else {
			diagnostic.Status, diagnostic.ManagementKeyEnv = bridge.ConfigurationStatus(), wrapper.ManagementKeyEnv
		}
		return response(200, struct {
			core.HostCapabilities
			QuotaBridge quotaBridgeDiagnostic `json:"quota_bridge"`
		}{capabilities, diagnostic}), nil
	case "POST /quota-connection-check":
		if stopping {
			return response(503, map[string]string{"error": "scheduler stopping"}), nil
		}
		if wrapper.quotaTransport() != "host" {
			return response(409, map[string]string{"error": "host_transport_required"}), nil
		}
		s.mu.RLock()
		host := s.host
		s.mu.RUnlock()
		checker, ok := host.(interface {
			CheckQuotaConnection(context.Context) error
			QuotaConnectionStatus() string
		})
		if !ok {
			return response(501, map[string]string{"error": "connection_check_unavailable"}), nil
		}
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		err := checker.CheckQuotaConnection(checkCtx)
		if publisher, ok := engine.(interface{ RefreshQuotaConnectionStatus() }); ok {
			publisher.RefreshQuotaConnectionStatus()
		}
		result := map[string]string{"status": checker.QuotaConnectionStatus()}
		if err != nil {
			code := "quota_host_operation_failed"
			switch err.Error() {
			case "quota_query_timeout", "quota_query_cancelled":
				code = err.Error()
			}
			result["error"] = code
		}
		return response(200, result), nil
	case "POST /reload":
		if stopping {
			return response(409, map[string]string{"error": "scheduler stopping"}), nil
		}
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.RLock()
		stopping = s.stopping
		wrapper = s.wrapper
		s.mu.RUnlock()
		if stopping {
			return response(409, map[string]string{"error": "scheduler stopping"}), nil
		}
		updated, err := s.options.LoadConfig(wrapper.ConfigFile)
		if err != nil || updated.Validate() != nil {
			return response(400, map[string]string{"error": "business configuration failed to load or validate"}), nil
		}
		if updated.Activity.IdentitySource != "host_caller_scope" {
			return response(400, map[string]string{"error": "identity_source_not_supported_by_cpa_adapter"}), nil
		}
		if err := engine.UpdateConfig(wrapper.effective(updated)); err != nil {
			return response(409, map[string]string{"error": "configuration update failed"}), nil
		}
		s.mu.Lock()
		s.cfg = updated
		s.mu.Unlock()
		return response(200, map[string]bool{"reloaded": true}), nil
	case "POST /recheck":
		if stopping {
			return response(409, map[string]string{"error": "scheduler stopping"}), nil
		}
		engine.Notify()
		return response(202, map[string]string{"status": "scheduled"}), nil
	default:
		return response(404, map[string]string{"error": "not found"}), nil
	}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type secureRandom struct{}

func (secureRandom) Duration(min, max time.Duration) (time.Duration, error) {
	if min < 0 || max < min {
		return 0, errors.New("invalid delay")
	}
	span := new(big.Int).SetInt64(int64(max - min))
	span.Add(span, big.NewInt(1))
	v, err := rand.Int(rand.Reader, span)
	if err != nil {
		return 0, err
	}
	return min + time.Duration(v.Int64()), nil
}
