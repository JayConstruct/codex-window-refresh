package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type candidateGenerator interface {
	GenerateCandidates(context.Context, store.Account, string, int) ([]string, error)
}

func (s *Service) generationSupported() bool {
	_, ok := s.source.(candidateGenerator)
	if source, available := s.source.(interface{ GenerationAvailable() bool }); available {
		return ok && source.GenerationAvailable()
	}
	return ok
}

func (s *Service) promptGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id := r.URL.Query().Get("job_id")
		if !store.ValidID(id) {
			apiError(w, 400, "job_id_required")
			return
		}
		generation, err := s.Store.Generation(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				apiError(w, 404, "not_found")
			} else {
				apiError(w, 503, "storage_unavailable")
			}
			return
		}
		generation.InstanceID = s.instance
		writeJSON(w, 200, generation)
		return
	}
	if !s.generationSupported() {
		apiError(w, 503, "prompt_generation_unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request store.GenerationRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Validate() != nil {
		apiError(w, 400, "invalid_prompt_generation")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(char rune) bool { return char < 33 || char > 126 }) >= 0 {
		apiError(w, 400, "idempotency_key_required")
		return
	}
	generation, _, err := s.Store.CreateGeneration(r.Context(), key, request)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			apiError(w, 409, "idempotency_conflict")
		case errors.Is(err, store.ErrBusy):
			apiError(w, 429, "queue_full")
		case errors.Is(err, store.ErrGenerationAccount):
			apiError(w, 409, "generation_account_unavailable")
		case errors.Is(err, store.ErrInvalidGeneration):
			apiError(w, 400, "invalid_prompt_generation")
		default:
			apiError(w, 503, "storage_unavailable")
		}
		return
	}
	generation.InstanceID = s.instance
	select {
	case s.generationWake <- struct{}{}:
	default:
	}
	writeJSON(w, 202, generation)
}

func generationOutcome(err error) (string, string) {
	if err == nil {
		return "completed", ""
	}
	switch err.Error() {
	case "generation_response_invalid", "generation_candidates_invalid":
		return "failed", "generation_response_invalid"
	case "account_identity_ambiguous", "account_identity_unavailable", "account_binding_changed", "generation_account_unavailable", "account_not_found", "account_disabled":
		return "failed", "generation_account_unavailable"
	case "generation_request_rejected", "generation_request_invalid", "generation_not_supported":
		return "failed", "generation_request_rejected"
	}
	return "unknown", "generation_outcome_unknown"
}

func (s *Service) generate(ctx context.Context) {
	generator, ok := s.source.(candidateGenerator)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		generation, err := s.Store.ClaimGeneration(ctx)
		if err != nil {
			s.generationError.Store(!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrBusy))
			select {
			case <-ctx.Done():
				return
			case <-s.generationWake:
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		s.generationError.Store(false)
		if generation.Phase != "running" {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		candidates, err := generator.GenerateCandidates(requestCtx, generation.Account, generation.Model, generation.Count)
		if requestCtx.Err() != nil {
			err = requestCtx.Err()
		}
		cancel()
		if err == nil {
			candidates, err = store.NormalizeGenerationCandidates(candidates, generation.Count)
		}
		phase, code := generationOutcome(err)
		if err != nil {
			candidates = nil
		}
		for {
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			saveErr := s.Store.FinishGeneration(saveCtx, generation.ID, phase, code, candidates)
			saveCancel()
			s.generationError.Store(saveErr != nil)
			if saveErr == nil || ctx.Err() != nil {
				break
			}
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}
