// Package events defines the private, allowlisted lifecycle observation wire format.
package events

import (
	"encoding/json"
	"net/http"
	"time"
)

const BatchLimit = 128

type Event struct {
	Sequence    uint64          `json:"sequence"`
	Kind        string          `json:"kind"`
	RequestID   string          `json:"request_id"`
	Identity    string          `json:"identity,omitempty"`
	AccountID   string          `json:"account_id,omitempty"`
	At          time.Time       `json:"at"`
	AuthIndex   string          `json:"auth_index,omitempty"`
	Binding     string          `json:"binding,omitempty"`
	Source      string          `json:"source,omitempty"`
	Headers     http.Header     `json:"headers,omitempty"`
	Quota       json.RawMessage `json:"quota,omitempty"`
	Project     string          `json:"project,omitempty"`
	ProjectName string          `json:"project_name,omitempty"`
	Traffic     *Traffic        `json:"traffic,omitempty"`
}

// Traffic contains only allowlisted execution metadata. Credentials, headers,
// prompts and response bodies must never be placed in this observation.
type Traffic struct {
	RequestedAt   string         `json:"requested_at,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	CompletedAt   string         `json:"completed_at,omitempty"`
	Phase         string         `json:"phase"`
	SelectionID   string         `json:"selection_id,omitempty"`
	ExecutionID   string         `json:"execution_id,omitempty"`
	TraceID       string         `json:"trace_id,omitempty"`
	Caller        string         `json:"caller,omitempty"`
	Session       string         `json:"session,omitempty"`
	ParentSession string         `json:"parent_session,omitempty"`
	Project       string         `json:"project,omitempty"`
	Model         string         `json:"model,omitempty"`
	AccountID     string         `json:"account_id,omitempty"`
	Provider      string         `json:"provider,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Status        string         `json:"status,omitempty"`
	HTTPStatus    *int           `json:"http_status,omitempty"`
	LatencyNS     *int64         `json:"latency_ns,omitempty"`
	TTFTNS        *int64         `json:"ttft_ns,omitempty"`
	InputTokens   *int64         `json:"input_tokens,omitempty"`
	OutputTokens  *int64         `json:"output_tokens,omitempty"`
	CachedTokens  *int64         `json:"cached_tokens,omitempty"`
	Evidence      map[string]any `json:"evidence,omitempty"`
}
type Batch struct {
	Instance     string  `json:"bridge_instance_id"`
	HostProcess  string  `json:"host_process_id"`
	After        uint64  `json:"after_sequence"`
	Earliest     uint64  `json:"earliest_sequence"`
	Latest       uint64  `json:"latest_sequence"`
	Acknowledged uint64  `json:"acknowledged_sequence"`
	LossEpoch    uint64  `json:"loss_epoch"`
	Gap          bool    `json:"gap"`
	Events       []Event `json:"events"`
}
