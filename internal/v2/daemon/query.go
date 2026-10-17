package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

func (service *Service) schedule(ctx context.Context) {
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := service.Store.ScheduleQueries(step, time.Now().UTC())
		cancel()
		if err != nil {
			if errors.Is(err, store.ErrBusy) {
				service.schedulerError.Store("queue_full")
			} else {
				service.schedulerError.Store("automatic_query_failed")
			}
		} else {
			service.schedulerError.Store("")
		}
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func (service *Service) querySettings(w http.ResponseWriter, r *http.Request) {
	var settings store.QuerySettings
	var err error
	if r.Method == http.MethodGet {
		settings, err = service.Store.QuerySettings(r.Context())
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		var request struct {
			Revision *uint64             `json:"revision"`
			Config   *policy.QueryConfig `json:"config"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Revision == nil || request.Config == nil {
			apiError(w, 400, "invalid_query_settings")
			return
		}
		settings, err = service.Store.SaveQuerySettings(r.Context(), *request.Revision, *request.Config)
	}
	if err != nil {
		status := 503
		if errors.Is(err, store.ErrSettingsConflict) {
			status = 409
		}
		if errors.Is(err, policy.ErrInvalid) {
			status = 400
		}
		apiError(w, status, store.PublicError(err))
		return
	}
	writeJSON(w, 200, map[string]any{"instance_id": service.instance, "view_revision": settings.ViewRevision, "revision": settings.Revision, "config": settings.Config})
}
