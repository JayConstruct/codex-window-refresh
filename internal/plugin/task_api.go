package plugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"time"

	"codex-window-refresh/internal/core"
	scheduler "codex-window-refresh/internal/runtime"
)

func historyResponse(engine Engine, values url.Values) managementResponse {
	filter := scheduler.HistoryFilter{Limit: 50}
	invalid := func() managementResponse { return response(400, map[string]string{"error": "invalid_history_filter"}) }
	for key, entries := range values {
		if len(entries) != 1 {
			return invalid()
		}
		value := entries[0]
		switch key {
		case "account_id":
			if len(value) > 512 {
				return invalid()
			}
			filter.AccountID = core.AccountID(value)
		case "kind":
			if value != "" && value != "query" && value != "wake" && value != "generation" && value != "reset" {
				return invalid()
			}
			filter.Kind = value
		case "status":
			allowed := map[string]bool{"": true, "completed": true, "failed": true, "skipped": true, "dispatched": true, "verifying": true, "verified": true, "unknown": true}
			if !allowed[value] {
				return invalid()
			}
			filter.Status = value
		case "cursor":
			cursor, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return invalid()
			}
			filter.Cursor = cursor
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				return invalid()
			}
			filter.Limit = limit
		case "from", "until":
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return invalid()
			}
			if key == "from" {
				filter.From = at
			} else {
				filter.Until = at
			}
		default:
			return invalid()
		}
	}
	if !filter.From.IsZero() && !filter.Until.IsZero() && filter.From.After(filter.Until) {
		return invalid()
	}
	reader, ok := engine.(interface {
		History(scheduler.HistoryFilter) scheduler.HistoryPage
	})
	if !ok {
		return response(501, map[string]string{"error": "history_unavailable"})
	}
	return response(200, reader.History(filter))
}

func generationResponse(engine Engine, wrapper WrapperConfig, done <-chan struct{}, stopping bool, body []byte) managementResponse {
	if stopping || done == nil || !wrapper.hostEnabled() {
		return response(409, map[string]string{"error": "scheduler_not_running"})
	}
	select {
	case <-done:
		return response(409, map[string]string{"error": "scheduler_not_running"})
	default:
	}
	if len(body) > 4096 {
		return response(400, map[string]string{"error": "invalid_generation_parameters"})
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	check := json.NewDecoder(bytes.NewReader(body))
	first, err := check.Token()
	if err != nil || first != json.Delim('{') {
		return response(400, map[string]string{"error": "invalid_generation_parameters"})
	}
	seen := map[string]bool{}
	for check.More() {
		key, err := check.Token()
		name, ok := key.(string)
		var value json.RawMessage
		if err != nil || !ok || seen[name] || check.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return response(400, map[string]string{"error": "invalid_generation_parameters"})
		}
		seen[name] = true
	}
	var request core.PromptGenerationRequest
	if decoder.Decode(&request) != nil {
		return response(400, map[string]string{"error": "invalid_generation_parameters"})
	}
	if _, err := decoder.Token(); err != io.EOF {
		return response(400, map[string]string{"error": "invalid_generation_parameters"})
	}
	generator, ok := engine.(interface {
		QueuePromptGeneration(core.PromptGenerationRequest) string
	})
	if !ok {
		return response(501, map[string]string{"error": "prompt_generation_unavailable"})
	}
	status := generator.QueuePromptGeneration(request)
	if status == "invalid_generation_parameters" {
		return response(400, map[string]string{"error": status})
	}
	if status != "queued" && status != "already_queued" {
		return response(409, map[string]string{"error": status})
	}
	return response(202, map[string]string{"status": status, "request_id": request.RequestID, "job_id": request.RequestID})
}
