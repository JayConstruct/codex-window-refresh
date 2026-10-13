package plugin

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func manualWakeResponse(engine Engine, wrapper WrapperConfig, cfg config.Config, done <-chan struct{}, stopping bool, body []byte) managementResponse {
	if stopping || done == nil || !wrapper.hostEnabled() {
		return response(409, map[string]string{"error": "scheduler_not_running"})
	}
	select {
	case <-done:
		return response(409, map[string]string{"error": "scheduler_not_running"})
	default:
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return response(400, map[string]string{"error": "invalid_manual_wake_parameters"})
	}
	var target core.AccountID
	field, err := decoder.Token()
	if err != nil || field != "account_id" || decoder.Decode(&target) != nil || strings.TrimSpace(string(target)) == "" || decoder.More() {
		return response(400, map[string]string{"error": "invalid_manual_wake_parameters"})
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return response(400, map[string]string{"error": "invalid_manual_wake_parameters"})
	}
	if _, err = decoder.Token(); err != io.EOF {
		return response(400, map[string]string{"error": "invalid_manual_wake_parameters"})
	}
	account, exists := engine.Snapshot().Accounts[target]
	if !exists {
		return response(404, map[string]string{"error": "account_not_found"})
	}
	if !account.Enabled || cfg.Account(target).Paused {
		return response(409, map[string]string{"error": "account_disabled"})
	}
	if strings.TrimSpace(cfg.Execution.Model) == "" {
		return response(409, map[string]string{"error": "model_required"})
	}
	if account.Task != nil && account.Task.OperationID != "" {
		return response(409, map[string]string{"error": "delivery_evidence_pending"})
	}
	debug, ok := engine.(DiagnosticEngine)
	if !ok {
		return response(501, map[string]string{"error": "manual_wake_unavailable"})
	}
	capabilities, _, _ := debug.Diagnostics()
	if !capabilities.StableAccountIdentity || !capabilities.ExactAccountExecution || !capabilities.QuotaAdapterReady {
		return response(503, map[string]string{"error": "host_capability_unavailable"})
	}
	wake, ok := engine.(interface{ QueueManualWake(core.AccountID) string })
	if !ok {
		return response(501, map[string]string{"error": "manual_wake_unavailable"})
	}
	status := wake.QueueManualWake(target)
	if status != "queued" && status != "already_queued" {
		return response(409, map[string]string{"error": status})
	}
	return response(202, map[string]string{"status": status, "account_id": string(target)})
}
