package cpa

import (
	"encoding/json"
	"errors"
	"time"

	"codex-window-refresh/internal/core"
)

var ErrActivityMalformed = errors.New("activity_callback_malformed")

func metadataString(values map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(values[key], &value)
	return value
}

// ParseActivity only accepts trusted host callback payloads, never request-body
// metadata or an API client-supplied user header. Hosts must invoke the lifecycle
// callbacks with the original authenticated metadata and stable RequestID.
func ParseActivity(method string, payload []byte, accounts map[core.AccountID]core.HostAccount) ([]core.ActivityEvent, error) {
	var request requestMetadata
	if json.Unmarshal(payload, &request) != nil {
		return nil, ErrActivityMalformed
	}
	if metadataString(request.Metadata, "source") == "plugin_host_model_callback" {
		return nil, nil
	}
	if request.RequestID == "" {
		return nil, ErrActivityMalformed
	}
	identity := core.UserIdentity(metadataString(request.Metadata, "caller_scope"))
	event := core.ActivityEvent{RequestID: core.RequestID(request.RequestID), Identity: identity, At: time.Now().UTC()}
	switch method {
	case "request.intercept_before":
		if identity == "" {
			return nil, nil
		}
		event.Phase = core.ActivityStarted
	case "request.intercept_after":
		event.Phase = core.ActivityAccountSelected
		id := core.AccountID(metadataString(request.Metadata, "selected_auth_id"))
		a, ok := accounts[id]
		if !ok {
			return nil, nil
		}
		event.AccountID = id
		event.Generation = a.Generation
	case "request.complete":
		event.Phase = core.ActivityCompleted
		if !request.CompletedAt.IsZero() {
			event.At = request.CompletedAt
		}
	default:
		return nil, ErrActivityMalformed
	}
	return []core.ActivityEvent{event}, nil
}

// PassThrough returns the untouched interceptor headers/body with no termination.
// Completion acknowledgments use an empty object. No request content is retained.
func PassThrough(method string, payload []byte) (any, error) {
	switch method {
	case "request.intercept_before", "request.intercept_after":
		var request requestMetadata
		if json.Unmarshal(payload, &request) != nil {
			return nil, ErrActivityMalformed
		}
		return struct {
			Headers map[string][]string
			Body    []byte
		}{request.Headers, request.Body}, nil
	case "request.complete":
		return struct{}{}, nil
	default:
		return nil, ErrActivityMalformed
	}
}
