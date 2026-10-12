package cpa

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"codex-window-refresh/internal/core"
)

const responseBindingTTL = 10 * time.Minute
const maxResponseBindings = 4096

type responseBinding struct {
	account     core.HostAccount
	expires     time.Time
	headersSeen bool
}

// ResponseQuotaObserver retains only request/account bindings, never prompts,
// response bodies, credentials or stream history. Callbacks perform no I/O.
// Binding at credential selection prevents a late response being attributed to
// a replacement credential. Completion is asynchronous and may precede the final
// response interceptor, so completed bindings remain briefly available.
type ResponseQuotaObserver struct {
	mu       sync.Mutex
	bindings map[string]responseBinding
}

func (o *ResponseQuotaObserver) Track(method string, payload []byte, accounts map[core.AccountID]core.HostAccount, at time.Time) {
	var req struct {
		RequestID string
		Metadata  map[string]json.RawMessage
	}
	if json.Unmarshal(payload, &req) != nil || req.RequestID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bindings == nil {
		o.bindings = make(map[string]responseBinding)
	}
	switch method {
	case "request.intercept_before":
		delete(o.bindings, req.RequestID)
	case "request.intercept_after":
		// Every retry replaces the binding, including a retry to an unsupported
		// provider or an account whose identity could not be established.
		delete(o.bindings, req.RequestID)
		if metadataString(req.Metadata, "source") == "plugin_host_model_callback" {
			return
		}
		id := core.AccountID(metadataString(req.Metadata, "selected_auth_id"))
		a, ok := accounts[id]
		if !ok || a.ID != id || a.Generation == "" {
			return
		}
		for id, b := range o.bindings {
			if !at.Before(b.expires) {
				delete(o.bindings, id)
			}
		}
		if len(o.bindings) >= maxResponseBindings {
			var oldest string
			var expiry time.Time
			for id, b := range o.bindings {
				if oldest == "" || b.expires.Before(expiry) {
					oldest, expiry = id, b.expires
				}
			}
			delete(o.bindings, oldest)
		}
		o.bindings[req.RequestID] = responseBinding{account: a, expires: at.Add(responseBindingTTL)}
	case "request.complete":
		if b, ok := o.bindings[req.RequestID]; ok {
			b.expires = at.Add(2 * time.Minute)
			o.bindings[req.RequestID] = b
		}
	}
}

// Observe accepts only host-provided quota headers or provider-native quota
// events. Account metadata from the client body is never inspected. accounts is
// lazy because ordinary stream chunks must not clone the entire account pool.
func (o *ResponseQuotaObserver) Observe(method string, payload []byte, accounts func() map[core.AccountID]core.HostAccount, at time.Time) []core.QuotaObservation {
	var req struct {
		RequestID                   string
		ResponseHeaders             http.Header
		ChunkIndex                  int
		Metadata                    map[string]json.RawMessage
		Provider, AuthID, EventType string
		Payload                     []byte
	}
	if json.Unmarshal(payload, &req) != nil || req.RequestID == "" || metadataString(req.Metadata, "source") == "plugin_host_model_callback" {
		return nil
	}
	var raw []byte
	source := "cpa_response_headers"
	switch method {
	case "response.intercept_after":
		raw = quotaHeaders(req.ResponseHeaders)
	case "response.intercept_stream_chunk":
		// Header-init is -1 in schema 6. Headers on later chunks are the same
		// old snapshot and must not advance quota freshness or reset countdowns.
		if req.ChunkIndex != -1 {
			return nil
		}
		raw = quotaHeaders(req.ResponseHeaders)
	case "websocket.response_event":
		if req.Provider != "codex" || req.AuthID == "" {
			return nil
		}
		var event struct {
			Type    string
			Headers map[string]json.RawMessage
		}
		if len(req.Payload) > 1<<20 || json.Unmarshal(req.Payload, &event) != nil || (req.EventType != "" && event.Type != req.EventType) {
			return nil
		}
		switch event.Type {
		case "codex.rate_limits":
			raw = req.Payload
		case "error":
			headers := make(http.Header)
			for name, value := range event.Headers {
				var text string
				var values []string
				var number json.Number
				var flag bool
				switch {
				case json.Unmarshal(value, &text) == nil:
					headers[name] = []string{text}
				case json.Unmarshal(value, &values) == nil:
					headers[name] = values
				case json.Unmarshal(value, &number) == nil:
					headers[name] = []string{number.String()}
				case json.Unmarshal(value, &flag) == nil:
					headers[name] = []string{strconv.FormatBool(flag)}
				}
			}
			raw = quotaHeaders(headers)
		default:
			return nil
		}
		source = "cpa_websocket_quota"
	default:
		return nil
	}
	// Reject absent or malformed quota before touching account state.
	observation, err := ParseQuota(raw, core.QuotaRead{}, at, core.ObservationExecution, core.AccountState{})
	if err != nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.bindings[req.RequestID]
	if !ok {
		return nil
	}
	if !at.Before(b.expires) {
		delete(o.bindings, req.RequestID)
		return nil
	}
	id := b.account.ID
	selected := core.AccountID(metadataString(req.Metadata, "selected_auth_id"))
	if (selected != "" && selected != id) || (req.AuthID != "" && core.AccountID(req.AuthID) != id) {
		return nil
	}
	current, ok := accounts()[id]
	if !ok || current.Generation != b.account.Generation {
		return nil
	}
	if method != "websocket.response_event" {
		if b.headersSeen {
			return nil
		}
		b.headersSeen = true
		o.bindings[req.RequestID] = b
	}
	observation.AccountID, observation.Generation, observation.Source = id, b.account.Generation, source
	if observation.FiveHour != nil {
		observation.FiveHour.Source = source
	}
	if observation.Weekly != nil {
		observation.Weekly.Source = source
	}
	return []core.QuotaObservation{observation}
}
