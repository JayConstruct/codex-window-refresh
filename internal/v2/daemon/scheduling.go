package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
	"codex-window-refresh/internal/v2/store"
)

type routingPublisher interface {
	PublishRouting(context.Context, store.RoutingSnapshot) error
}
type resetSource interface {
	ReadResetCredits(context.Context, store.Account) (*policy.ResetCredits, error)
	ConsumeResetCredit(context.Context, store.Account, string, string) (string, error)
}

func (s *Service) publishRouting(ctx context.Context) {
	publisher, ok := s.source.(routingPublisher)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 3*time.Second)
		snapshot, err := s.Store.RoutingSnapshot(step, time.Now().UTC())
		if err == nil {
			err = publisher.PublishRouting(step, snapshot)
		}
		cancel()
		s.routingError.Store(err != nil)
		if !wait(ctx, 2*time.Second) {
			return
		}
	}
}
func resetCode(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "reset_codex_unavailable", "reset_runtime_unavailable", "reset_protocol_invalid", "reset_transport_failed", "reset_account_rpc_failed", "reset_verification_incomplete", "account_binding_changed", "account_disabled", "quota_proxy_invalid":
		return err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "reset_timeout"
	}
	return "reset_operation_failed"
}
func (s *Service) manageResetCards(ctx context.Context) {
	source, ok := s.source.(resetSource)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, time.Minute)
		err := s.resetStep(step, source)
		cancel()
		s.resetError.Store(err != nil && !errors.Is(err, store.ErrNotFound))
		if !wait(ctx, time.Second) {
			return
		}
	}
}
func (s *Service) resetStep(ctx context.Context, source resetSource) error {
	settings, err := s.Store.SchedulingSettings(ctx)
	if err != nil {
		return err
	}
	p := settings.Config.Cards
	job, err := s.Store.PendingReset(ctx, time.Now().UTC())
	if err == nil {
		if job.Phase == "verifying" {
			return s.verifyReset(ctx, source, job, p.PollSeconds)
		}
		if p.Enabled && p.AutoConsume {
			return s.consumeReset(ctx, source, job, p.PollSeconds)
		}
		err = store.ErrNotFound
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !p.Enabled {
		return nil
	}
	a, err := s.Store.NextResetAccount(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	credits, probeErr := source.ReadResetCredits(ctx, a)
	if err = s.Store.RecordResetCredits(ctx, a, credits, resetCode(probeErr), time.Now().UTC(), p.PollSeconds); err != nil {
		return err
	}
	if probeErr != nil {
		return probeErr
	}
	if !p.AutoConsume || credits == nil || credits.AvailableCount == 0 {
		return nil
	}
	// Refresh both quota windows before deciding whether a card is useful.
	raw, err := s.source.Query(ctx, a)
	if err != nil {
		return err
	}
	if err = s.Store.SaveResetQuota(ctx, a, raw); err != nil {
		return err
	}
	var selected *policy.ResetCredit
	for i := range credits.Credits {
		c := &credits.Credits[i]
		if c.Status == "available" && c.ResetType == "codexRateLimits" && c.ExpiresAt != nil && *c.ExpiresAt > time.Now().Unix() && (selected == nil || *c.ExpiresAt < *selected.ExpiresAt) {
			selected = c
		}
	}
	if selected == nil {
		return nil
	}
	job, err = s.Store.ClaimReset(ctx, a, *selected, time.Now().UTC())
	if err != nil {
		return err
	}
	return s.consumeReset(ctx, source, job, p.PollSeconds)
}
func (s *Service) consumeReset(ctx context.Context, source resetSource, job store.ResetAttempt, poll int) error {
	// The persisted ID is reused after timeouts and restarts, even if the
	// service already consumed the card before its response was lost.
	outcome, err := source.ConsumeResetCredit(ctx, job.Account, job.CreditID, job.ID)
	phase := "verifying"
	if err != nil {
		phase = "unknown"
	} else if outcome == "noCredit" || outcome == "nothingToReset" {
		phase = "skipped"
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	saveErr := s.Store.FinishReset(finishCtx, job, phase, outcome, resetCode(err), time.Now().UTC())
	cancel()
	if saveErr != nil {
		return saveErr
	}
	if err != nil {
		return err
	}
	job.Phase = phase
	job.Outcome = outcome
	if phase == "verifying" {
		return s.verifyReset(ctx, source, job, poll)
	}
	return nil
}
func (s *Service) verifyReset(ctx context.Context, source resetSource, job store.ResetAttempt, poll int) error {
	raw, err := s.source.Query(ctx, job.Account)
	if err == nil {
		var snapshot quota.Snapshot
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.FiveHour == nil || snapshot.Weekly == nil {
			err = errors.New("reset_verification_incomplete")
		}
	}
	if err == nil {
		var snapshot quota.Snapshot
		_ = json.Unmarshal(raw, &snapshot)
		if snapshot.Permission == nil || *snapshot.Permission != core.PermissionAllowed || snapshot.FiveHour.Remaining == nil || snapshot.Weekly.Remaining == nil || snapshot.FiveHour.Remaining.Value <= 0 || snapshot.Weekly.Remaining.Value <= 0 {
			err = errors.New("reset_verification_incomplete")
		}
	}
	if err == nil {
		if clearer, ok := s.source.(interface {
			ClearResetCooldown(context.Context, store.Account) error
		}); ok {
			err = clearer.ClearResetCooldown(ctx, job.Account)
			if err == nil && s.options.RequestInventorySync != nil {
				s.options.RequestInventorySync()
			}
		}
	}
	if err == nil {
		err = s.Store.SaveResetQuota(ctx, job.Account, raw)
	}
	if err == nil {
		var cards *policy.ResetCredits
		cards, err = source.ReadResetCredits(ctx, job.Account)
		if err == nil {
			err = s.Store.RecordResetCredits(ctx, job.Account, cards, "", time.Now().UTC(), poll)
		}
	}
	phase := "completed"
	if err != nil {
		phase = "verifying"
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.Store.FinishReset(finishCtx, job, phase, job.Outcome, resetCode(err), time.Now().UTC()); e != nil {
		return e
	}
	return err
}
func (s *Service) scheduling(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		var request struct {
			Revision *uint64                  `json:"revision"`
			Config   *policy.SchedulingConfig `json:"config"`
		}
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || request.Revision == nil || request.Config == nil {
			apiError(w, 400, "invalid_scheduling_settings")
			return
		}
		settings, err := s.Store.SaveScheduling(r.Context(), *request.Revision, *request.Config)
		if err != nil {
			status := 503
			code := "storage_unavailable"
			if errors.Is(err, policy.ErrScheduling) {
				status = 400
				code = err.Error()
			}
			if errors.Is(err, store.ErrSettingsConflict) {
				status = 409
				code = err.Error()
			}
			apiError(w, status, code)
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	settings, err := s.Store.SchedulingSettings(r.Context())
	if err != nil {
		apiError(w, 503, "storage_unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"instance_id": s.instance, "revision": settings.Revision, "config": settings.Config, "routing_error": s.routingError.Load(), "reset_error": s.resetError.Load()})
}
func (s *Service) resetCards(w http.ResponseWriter, r *http.Request) {
	cards, attempts, err := s.Store.ResetViews(r.Context())
	if err != nil {
		apiError(w, 503, "storage_unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"instance_id": s.instance, "accounts": cards, "attempts": attempts})
}
