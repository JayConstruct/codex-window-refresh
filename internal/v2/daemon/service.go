// Package daemon hosts quota queries, candidate generation and opt-in model wakeups.
package daemon

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-window-refresh/internal/v2/executor"
	"codex-window-refresh/internal/v2/store"
	"codex-window-refresh/internal/v2/version"
)

//go:embed web/index.html
var html []byte

//go:embed web/panel.js
var javascript []byte

//go:embed web/panel.css
var stylesheet []byte

type Options struct {
	QueryConcurrency     int
	QueryTimeout         time.Duration
	Retention            store.Retention
	RequestInventorySync func() bool
}
type Service struct {
	Store              *store.Store
	source             executor.Source
	options            Options
	instance           string
	wake               chan struct{}
	generationWake     chan struct{}
	generationError    atomic.Bool
	wakeWork           chan struct{}
	wakeWorkerError    atomic.Bool
	wakeSchedulerError atomic.Bool
	expanderError      atomic.Value
	maintenanceError   atomic.Bool
	schedulerError     atomic.Value
	workerErrors       []atomic.Bool
}

func New(st *store.Store, source executor.Source, o Options) (*Service, error) {
	if o.Retention.HistoryAge == 0 && o.Retention.TerminalJobs == 0 {
		o.Retention = store.DefaultRetention()
	}
	if st == nil || source == nil || o.QueryConcurrency < 1 || o.QueryConcurrency > 128 || o.QueryTimeout < time.Second || o.QueryTimeout > 5*time.Minute {
		return nil, errors.New("invalid_backend_options")
	}
	if o.Retention.HistoryAge < time.Hour || o.Retention.HistoryAge > 365*24*time.Hour || o.Retention.TerminalJobs < 1 || o.Retention.TerminalJobs > 50000 {
		return nil, errors.New("invalid_retention_options")
	}
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := st.RecoverGenerations(recoverCtx)
	if err == nil {
		err = st.RecoverWakes(recoverCtx)
	}
	recoverCancel()
	if err != nil {
		return nil, err
	}
	s := &Service{Store: st, source: source, options: o, instance: store.ID(), wake: make(chan struct{}, 1), generationWake: make(chan struct{}, 1), wakeWork: make(chan struct{}, 1), workerErrors: make([]atomic.Bool, o.QueryConcurrency)}
	s.expanderError.Store("")
	s.schedulerError.Store("")
	return s, nil
}
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (s *Service) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); s.maintenance(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.expand(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.schedule(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.generate(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.scheduleWakes(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.executeWakes(ctx) }()
	for i := 0; i < s.options.QueryConcurrency; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); s.worker(ctx, i) }()
	}
	workers.Wait()
}
func (s *Service) maintenance(ctx context.Context) {
	for ctx.Err() == nil {
		prune, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.Store.Prune(prune, time.Now(), s.options.Retention)
		cancel()
		s.maintenanceError.Store(err != nil)
		if !wait(ctx, time.Minute) {
			return
		}
	}
}
func (s *Service) lastError() string {
	if s.wakeWorkerError.Load() {
		return "storage_unavailable"
	}
	if s.wakeSchedulerError.Load() {
		return "automatic_wake_failed"
	}
	if s.generationError.Load() {
		return "storage_unavailable"
	}
	if code := s.schedulerError.Load().(string); code != "" {
		return code
	}
	if s.maintenanceError.Load() {
		return "storage_unavailable"
	}
	for i := range s.workerErrors {
		if s.workerErrors[i].Load() {
			return "storage_unavailable"
		}
	}
	return s.expanderError.Load().(string)
}
func publicCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "quota_query_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "query_interrupted"
	}
	switch err.Error() {
	case "bridge_unavailable", "bridge_busy", "bridge_instance_changed", "bridge_transport_failed", "bridge_protocol_incompatible", "bridge_response_invalid", "host_callback_failed", "inventory_too_large", "account_identity_ambiguous", "account_identity_unavailable", "account_binding_changed", "quota_credential_missing", "quota_proxy_invalid", "quota_transport_failed", "quota_authentication_failed", "quota_upstream_forbidden", "quota_upstream_rate_limited", "quota_upstream_http_failed", "quota_body_failed", "quota_response_too_large", "quota_response_unrecognized", "quota_clock_changed":
		return err.Error()
	}
	return "quota_query_failed"
}
func (s *Service) expand(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := s.Store.Pending(ctx)
		if errors.Is(err, store.ErrNotFound) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			s.expanderError.Store("storage_unavailable")
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, s.options.QueryTimeout)
		accounts, err := s.source.Accounts(readCtx)
		if err == nil && readCtx.Err() != nil {
			err = readCtx.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			code := publicCode(err)
			s.expanderError.Store(code)
			if e := s.Store.Block(ctx, job.ID, code); e != nil {
				s.expanderError.Store("storage_unavailable")
			}
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		if err = s.Store.Expand(ctx, job, accounts); err != nil {
			s.expanderError.Store("storage_unavailable")
			if !wait(ctx, time.Second) {
				return
			}
		} else {
			s.expanderError.Store("")
		}
	}
}
func (s *Service) worker(ctx context.Context, index int) {
	for ctx.Err() == nil {
		run, err := s.Store.Claim(ctx)
		if err != nil {
			s.workerErrors[index].Store(!errors.Is(err, store.ErrNotFound))
			if !wait(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}
		s.workerErrors[index].Store(false)
		readCtx, cancel := context.WithTimeout(ctx, s.options.QueryTimeout)
		quota, err := s.source.Query(readCtx, run.Account)
		if readCtx.Err() != nil {
			err = readCtx.Err()
		}
		cancel()
		code := ""
		if err != nil {
			code = publicCode(err)
		}
		// Cancellation must not erase an outcome already obtained. Query failure is
		// safe to retry after a restart; committed job identity is always retained.
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		saveErr := s.Store.Finish(saveCtx, run, quota, code)
		saveCancel()
		if saveErr != nil {
			s.workerErrors[index].Store(true)
			// Do not claim more work while this result is not committed. Retain just
			// this bounded result per worker; restart requeues interrupted reads.
			for ctx.Err() == nil && saveErr != nil {
				if !wait(ctx, time.Second) {
					return
				}
				saveCtx, saveCancel = context.WithTimeout(ctx, 5*time.Second)
				saveErr = s.Store.Finish(saveCtx, run, quota, code)
				saveCancel()
			}
		}
		if saveErr == nil {
			s.workerErrors[index].Store(false)
		}
	}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serve) }
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var data []byte
		kind := ""
		switch r.URL.Path {
		case "/ui/status":
			data, kind = html, "text/html; charset=utf-8"
		case "/ui/panel.js":
			data, kind = javascript, "text/javascript; charset=utf-8"
		case "/ui/panel.css":
			data, kind = stylesheet, "text/css; charset=utf-8"
		}
		if kind != "" {
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
			w.Write(data)
			return
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v2/overview":
		view, err := s.Store.Overview(r.Context())
		if err != nil {
			apiError(w, 503, "storage_unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"version": version.Version, "instance_id": s.instance, "view_revision": view.Revision, "accounts_revision": view.AccountsRevision, "activity": view.Activity, "inventory": view.Inventory, "dashboard": view.Dashboard, "query_settings": view.QuerySettings, "protocol_version": 1, "read_only": true, "query_concurrency": s.options.QueryConcurrency, "last_error": s.lastError(), "capabilities": map[string]any{"quota_query": true, "model_execution": s.wakeSupported(), "wake_settings": true, "automatic_wake": s.wakeSupported(), "prompt_generation": s.generationSupported(), "editable_generation_prompt": s.editableGenerationPrompt(), "generation_accounts": s.generationAccountsSupported(), "prompt_generation_providers": s.generationProviders(), "model_settings": true, "activity_tracking": true, "passive_quota": true, "inventory_sync": s.options.RequestInventorySync != nil, "query_settings": true, "automatic_quota_query": true, "randomized_quota_query": true}, "retention": map[string]any{"history_seconds": s.options.Retention.HistoryAge.Seconds(), "terminal_jobs": s.options.Retention.TerminalJobs, "idempotency_seconds": 30 * 24 * 60 * 60}})
	case "GET /api/v2/wake-settings", "POST /api/v2/wake-settings":
		s.wakeSettings(w, r)
	case "GET /api/v2/wake-jobs", "POST /api/v2/wake-jobs":
		s.wakeJobs(w, r)
	case "GET /api/v2/model-settings", "POST /api/v2/model-settings":
		s.modelSettings(w, r)
	case "GET /api/v2/prompt-generations", "POST /api/v2/prompt-generations":
		s.promptGenerations(w, r)
	case "GET /api/v2/generation-accounts":
		s.generationAccounts(w, r)
	case "GET /api/v2/settings", "POST /api/v2/settings":
		s.querySettings(w, r)
	case "POST /api/v2/sync":
		if s.options.RequestInventorySync == nil || !s.options.RequestInventorySync() {
			apiError(w, 503, "inventory_sync_unavailable")
			return
		}
		writeJSON(w, 202, map[string]any{"status": "queued", "read_only": true, "instance_id": s.instance})
	case "GET /healthz":
		if err := s.Store.Check(r.Context()); err != nil {
			apiError(w, 503, "storage_unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ready", "version": version.Version, "read_only": true, "instance_id": s.instance})
	case "POST /api/v2/jobs":
		s.createJob(w, r)
	case "GET /api/v2/jobs":
		id := r.URL.Query().Get("job_id")
		if id == "" && !r.URL.Query().Has("job_id") {
			limit := 20
			if raw := r.URL.Query().Get("limit"); raw != "" {
				var err error
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 1 || limit > 100 {
					apiError(w, 400, "invalid_limit")
					return
				}
			}
			page, err := s.Store.RecentJobsPage(r.Context(), limit)
			if err != nil {
				apiError(w, 503, "storage_unavailable")
				return
			}
			writeJSON(w, 200, map[string]any{"jobs": page.Jobs, "view_revision": page.Revision, "instance_id": s.instance})
			return
		}
		if !store.ValidID(id) {
			apiError(w, 400, "job_id_required")
			return
		}
		j, err := s.Store.Job(r.Context(), id)
		if err != nil {
			status := 503
			if errors.Is(err, store.ErrNotFound) {
				status = 404
			}
			apiError(w, status, store.PublicError(err))
			return
		}
		j.InstanceID = s.instance
		writeJSON(w, 200, j)
	case "GET /api/v2/accounts":
		search, status := r.URL.Query().Get("search"), r.URL.Query().Get("status")
		if len(search) > 256 || strings.ContainsAny(search, "\x00\r\n") {
			apiError(w, 400, "invalid_search")
			return
		}
		switch status {
		case "", "all", "available", "disabled", "unavailable", "attention", "quota_exhausted", "quota_restricted":
		default:
			apiError(w, 400, "invalid_status")
			return
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 500 {
				apiError(w, 400, "invalid_limit")
				return
			}
			limit = n
		}
		var expected *uint64
		if raw := r.URL.Query().Get("revision"); raw != "" {
			revision, err := strconv.ParseUint(raw, 10, 53)
			if err != nil {
				apiError(w, 400, "invalid_revision")
				return
			}
			expected = &revision
		}
		if r.URL.Query().Get("after") != "" && expected == nil {
			apiError(w, 400, "revision_required")
			return
		}
		page, err := s.Store.AccountPageFiltered(r.Context(), r.URL.Query().Get("after"), limit, expected, search, status)
		if err != nil {
			status := 503
			if errors.Is(err, store.ErrViewChanged) {
				status = 409
			}
			apiError(w, status, store.PublicError(err))
			return
		}
		page.InstanceID = s.instance
		writeJSON(w, 200, page)
	default:
		apiError(w, 404, "not_found")
	}
}
func (s *Service) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Kind      string `json:"kind"`
		AccountID string `json:"account_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.Kind != "quota_query" || (req.AccountID != "" && !store.ValidID(req.AccountID)) {
		apiError(w, 400, "invalid_query_job")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
		apiError(w, 400, "idempotency_key_required")
		return
	}
	canonical, _ := json.Marshal(req)
	sum := sha256.Sum256(canonical)
	id, existing, err := s.Store.CreateQuery(r.Context(), key, hex.EncodeToString(sum[:]), req.AccountID)
	if err != nil {
		status := 503
		if errors.Is(err, store.ErrConflict) {
			status = 409
		}
		if errors.Is(err, store.ErrJobExpired) {
			status = 410
		}
		apiError(w, status, store.PublicError(err))
		return
	}
	s.notify()
	writeJSON(w, 202, map[string]any{"job_id": id, "existing": existing, "read_only": true, "instance_id": s.instance})
}
