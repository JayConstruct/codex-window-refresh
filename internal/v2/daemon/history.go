package daemon

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"codex-window-refresh/internal/v2/store"
)

func historyFilter(raw string) (store.TaskHistoryFilter, error) {
	filter := store.TaskHistoryFilter{Page: 1, Limit: 10}
	if len(raw) > 4096 {
		return filter, store.ErrInvalidTaskHistoryFilter
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return filter, store.ErrInvalidTaskHistoryFilter
	}
	fields := map[string]*string{"search": &filter.Search, "type": &filter.Type, "source": &filter.Source,
		"trigger": &filter.Trigger, "status": &filter.Status, "from": &filter.From, "to": &filter.To,
		"today_from": &filter.TodayFrom, "today_to": &filter.TodayTo}
	for name, entries := range values {
		if len(entries) != 1 {
			return filter, store.ErrInvalidTaskHistoryFilter
		}
		if field, ok := fields[name]; ok {
			*field = entries[0]
			continue
		}
		if name != "page" && name != "limit" {
			return filter, store.ErrInvalidTaskHistoryFilter
		}
		number, parseErr := strconv.Atoi(entries[0])
		if parseErr != nil || number < 1 || (name == "limit" && number > 100) {
			return filter, store.ErrInvalidTaskHistoryFilter
		}
		if name == "page" {
			filter.Page = number
		} else {
			filter.Limit = number
		}
	}
	return filter, nil
}

func (service *Service) taskHistory(w http.ResponseWriter, request *http.Request) {
	filter, err := historyFilter(request.URL.RawQuery)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_task_history_filter")
		return
	}
	page, err := service.Store.TaskHistory(request.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidTaskHistoryFilter) {
			apiError(w, http.StatusBadRequest, "invalid_task_history_filter")
		} else {
			apiError(w, http.StatusServiceUnavailable, "storage_unavailable")
		}
		return
	}
	page.InstanceID = service.instance
	writeJSON(w, http.StatusOK, page)
}
