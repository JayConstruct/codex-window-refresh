package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

type wakeExecutor interface {
	WakeAvailable() bool
	ExecuteWake(context.Context, store.Account, string, string) error
}

func (s *Service) wakeSupported() bool {
	source, ok := s.source.(wakeExecutor)
	return ok && source.WakeAvailable()
}

func (s *Service) notifyWake() {
	select {
	case s.wakeWork <- struct{}{}:
	default:
	}
}

func (s *Service) wakeSettings(w http.ResponseWriter, r *http.Request) {
	var settings store.WakeSettings
	var err error
	if r.Method == http.MethodGet {
		settings, err = s.Store.WakeSettings(r.Context())
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil || !utf8.Valid(raw) {
			apiError(w, 400, "invalid_wake_settings")
			return
		}
		var request struct {
			Revision *uint64            `json:"revision"`
			Config   *policy.WakeConfig `json:"config"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Revision == nil || request.Config == nil {
			apiError(w, 400, "invalid_wake_settings")
			return
		}
		settings, err = s.Store.SaveWakeSettings(r.Context(), *request.Revision, *request.Config)
	}
	if err != nil {
		wakeAPIError(w, err)
		return
	}
	if r.Method == http.MethodPost {
		s.notifyWake()
	}
	writeJSON(w, 200, map[string]any{"instance_id": s.instance, "view_revision": settings.ViewRevision, "revision": settings.Revision, "config": settings.Config})
}

func wakeAPIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict")
	case errors.Is(err, store.ErrNotFound):
		apiError(w, 404, "not_found")
	case errors.Is(err, store.ErrBusy):
		apiError(w, 429, "queue_full")
	case errors.Is(err, store.ErrWakeSettingsConflict):
		apiError(w, 409, "wake_settings_conflict")
	case errors.Is(err, policy.ErrInvalidWake):
		apiError(w, 400, "invalid_wake_settings")
	case errors.Is(err, store.ErrInvalidWakeRequest):
		apiError(w, 400, "invalid_wake_request")
	case errors.Is(err, store.ErrWakeAccount):
		apiError(w, 409, store.WakeAccountErrorCode(err))
	case errors.Is(err, store.ErrWakeModelMissing):
		apiError(w, 409, "wake_model_missing")
	default:
		apiError(w, 503, "storage_unavailable")
	}
}

func (s *Service) wakeJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if r.URL.Query().Has("job_id") {
			id := r.URL.Query().Get("job_id")
			if !store.ValidID(id) {
				apiError(w, 400, "job_id_required")
				return
			}
			job, err := s.Store.Wake(r.Context(), id)
			if err != nil {
				wakeAPIError(w, err)
				return
			}
			job.InstanceID = s.instance
			writeJSON(w, 200, job)
			return
		}
		limit := 20
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				apiError(w, 400, "invalid_limit")
				return
			}
		}
		page, err := s.Store.WakeJobs(r.Context(), limit)
		if err != nil {
			wakeAPIError(w, err)
			return
		}
		page.InstanceID = s.instance
		for index := range page.Jobs {
			page.Jobs[index].InstanceID = s.instance
		}
		writeJSON(w, 200, page)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(raw) {
		apiError(w, 400, "invalid_wake_request")
		return
	}
	var request store.WakeRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !store.ValidID(request.AccountID) {
		apiError(w, 400, "invalid_wake_request")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(char rune) bool { return char < 33 || char > 126 }) >= 0 {
		apiError(w, 400, "idempotency_key_required")
		return
	}
	job, existing, err := s.Store.LookupWake(r.Context(), key, request)
	if err == nil && !existing {
		if !s.wakeSupported() {
			apiError(w, 503, "wake_unavailable")
			return
		}
		job, _, err = s.Store.CreateWake(r.Context(), key, request)
	}
	if err != nil {
		wakeAPIError(w, err)
		return
	}
	job, err = s.Store.Wake(r.Context(), job.ID)
	if err != nil {
		wakeAPIError(w, err)
		return
	}
	job.InstanceID = s.instance
	s.notifyWake()
	writeJSON(w, 202, job)
}

func (s *Service) scheduleWakes(ctx context.Context) {
	for ctx.Err() == nil {
		if s.wakeSupported() {
			step, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := s.Store.ScheduleWakes(step, time.Now().UTC(), s.instance)
			cancel()
			s.wakeSchedulerError.Store(err != nil && !errors.Is(err, store.ErrBusy))
			if err == nil {
				s.notifyWake()
				s.notify()
			}
		} else {
			s.wakeSchedulerError.Store(false)
		}
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func wakeOutcome(err error) (string, string) {
	if err == nil {
		return "verifying", ""
	}
	if code := store.WakeAccountErrorCode(err); code != "" {
		return "failed", code
	}
	switch err.Error() {
	case "wake_model_busy":
		return "queued", "model_busy"
	case "account_identity_ambiguous":
		return "failed", "wake_account_ambiguous"
	case "account_binding_changed":
		return "failed", "wake_binding_changed"
	case "account_not_found":
		return "failed", "wake_account_not_found"
	case "account_disabled":
		return "failed", "wake_account_disabled"
	case "account_identity_unavailable":
		return "failed", "wake_account_unavailable"
	case "wake_not_supported", "generation_not_supported", "wake_request_invalid", "wake_request_rejected":
		return "failed", "wake_request_rejected"
	}
	return "unknown", "wake_outcome_unknown"
}

func (s *Service) executeWakes(ctx context.Context) {
	source, ok := s.source.(wakeExecutor)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		if !source.WakeAvailable() {
			if !s.waitWake(ctx) {
				return
			}
			continue
		}
		step, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, err := s.Store.PrepareWake(step, time.Now().UTC())
		cancel()
		if err == nil {
			step, cancel = context.WithTimeout(ctx, 5*time.Second)
			job, err = s.Store.AuthorizeWake(step, job.ID, time.Now().UTC())
			cancel()
		}
		if err != nil {
			s.wakeWorkerError.Store(!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrBusy))
			if !s.waitWake(ctx) {
				return
			}
			continue
		}
		s.wakeWorkerError.Store(false)
		if job.Phase != "sending" {
			if !s.waitWake(ctx) {
				return
			}
			continue
		}
		requestCtx, requestCancel := context.WithTimeout(ctx, 90*time.Second)
		response := store.WakeResponse{Stage: "preflight"}
		if recorder, ok := source.(interface {
			ExecuteWakeWithResponse(context.Context, store.Account, string, string) (store.WakeResponse, error)
		}); ok {
			response, err = recorder.ExecuteWakeWithResponse(requestCtx, job.Account, job.Model, job.Prompt)
		} else {
			err = source.ExecuteWake(requestCtx, job.Account, job.Model, job.Prompt)
			if err != nil {
				response.ErrorCode = err.Error()
			}
		}
		if requestCtx.Err() != nil {
			err = requestCtx.Err()
		}
		requestCancel()
		phase, code := wakeOutcome(err)
		finishedAt := time.Now().UTC()
		for {
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			saveErr := s.Store.FinishWakeWithResponse(saveCtx, job.ID, phase, code, finishedAt, &response)
			saveCancel()
			s.wakeWorkerError.Store(saveErr != nil)
			if saveErr == nil || ctx.Err() != nil {
				break
			}
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}

func (s *Service) waitWake(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wakeWork:
		return true
	case <-time.After(200 * time.Millisecond):
		return true
	}
}
