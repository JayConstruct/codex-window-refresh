package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

func (service *Service) modelSettings(w http.ResponseWriter, r *http.Request) {
	var settings store.ModelSettings
	var err error
	if r.Method == http.MethodGet {
		settings, err = service.Store.ModelSettings(r.Context())
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil || !utf8.Valid(raw) {
			apiError(w, 400, "invalid_model_settings")
			return
		}
		var request struct {
			Revision *uint64             `json:"revision"`
			Config   *policy.ModelConfig `json:"config"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Revision == nil || request.Config == nil {
			apiError(w, 400, "invalid_model_settings")
			return
		}
		settings, err = service.Store.SaveModelSettings(r.Context(), *request.Revision, *request.Config)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrModelSettingsConflict):
			apiError(w, 409, "model_settings_conflict")
		case errors.Is(err, policy.ErrInvalidModelSettings):
			apiError(w, 400, "invalid_model_settings")
		default:
			apiError(w, 503, "storage_unavailable")
		}
		return
	}
	writeJSON(w, 200, map[string]any{"instance_id": service.instance, "view_revision": settings.ViewRevision, "revision": settings.Revision, "config": settings.Config})
}
