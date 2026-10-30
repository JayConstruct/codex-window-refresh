package daemon

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"codex-window-refresh/internal/v2/store"
)

func (s *Service) traffic(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > 4096 {
		apiError(w, 400, "invalid_traffic_filter")
		return
	}
	f := store.TrafficFilter{Page: 1, Limit: 10}
	fields := map[string]*string{"kind": &f.Kind, "id": &f.ID, "request_id": &f.ID, "search": &f.Search, "project": &f.Project, "caller": &f.Caller, "account": &f.Account, "model": &f.Model, "status": &f.Status, "from": &f.From, "to": &f.To, "session": &f.Session}
	values, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil || values.Has("id") && values.Has("request_id") {
		apiError(w, 400, "invalid_traffic_filter")
		return
	}
	for name, values := range values {
		if len(values) != 1 {
			apiError(w, 400, "invalid_traffic_filter")
			return
		}
		if field, ok := fields[name]; ok {
			*field = values[0]
			continue
		}
		if name != "page" && name != "limit" {
			apiError(w, 400, "invalid_traffic_filter")
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 {
			apiError(w, 400, "invalid_traffic_filter")
			return
		}
		if name == "page" {
			f.Page = n
		} else {
			f.Limit = n
		}
	}
	page, err := s.Store.Traffic(r.Context(), f)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTrafficFilter):
			apiError(w, 400, "invalid_traffic_filter")
		case errors.Is(err, store.ErrNotFound):
			apiError(w, 404, "record_not_found")
		default:
			apiError(w, 503, "storage_unavailable")
		}
		return
	}
	page.InstanceID = s.instance
	writeJSON(w, 200, page)
}
