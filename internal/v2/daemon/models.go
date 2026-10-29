package daemon

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/executor"
	"codex-window-refresh/internal/v2/store"
)

type accountModelDirectory interface {
	AccountModels(context.Context, string) (executor.AccountModelCatalog, error)
}

func (s *Service) accountModelsSupported() bool {
	_, supported := s.source.(accountModelDirectory)
	if source, ok := s.source.(interface{ AccountModelsAvailable() bool }); ok {
		return supported && source.AccountModelsAvailable()
	}
	return supported
}

func (s *Service) accountModels(w http.ResponseWriter, r *http.Request) {
	source, ok := s.source.(accountModelDirectory)
	if !ok || !s.accountModelsSupported() {
		apiError(w, 501, "model_catalog_unsupported")
		return
	}
	query := r.URL.Query()
	id := query.Get("account_id")
	if len(query) != 1 || len(query["account_id"]) != 1 || !store.ValidID(id) || strings.TrimSpace(id) == "" || !utf8.ValidString(id) || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		apiError(w, 400, "model_catalog_account_invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	catalog, err := source.AccountModels(ctx, query.Get("account_id"))
	if err != nil {
		switch err.Error() {
		case "model_catalog_unsupported":
			apiError(w, 501, "model_catalog_unsupported")
		case "model_catalog_changed", "model_catalog_account_invalid":
			apiError(w, 409, err.Error())
		default:
			apiError(w, 503, "model_catalog_unavailable")
		}
		return
	}
	if catalog.AccountID != query.Get("account_id") || (catalog.Provider != "codex" && catalog.Provider != "antigravity") || catalog.Models == nil || len(catalog.Models) > 2048 {
		apiError(w, 503, "model_catalog_unavailable")
		return
	}
	for _, model := range catalog.Models {
		if model == "" || len(model) > 200 || !utf8.ValidString(model) || strings.IndexFunc(model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			apiError(w, 503, "model_catalog_unavailable")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"instance_id": s.instance, "account_id": catalog.AccountID, "provider": catalog.Provider, "models": catalog.Models})
}
