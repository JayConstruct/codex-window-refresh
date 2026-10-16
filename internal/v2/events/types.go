// Package events defines the private, allowlisted lifecycle observation wire format.
package events

import (
	"encoding/json"
	"net/http"
	"time"
)

const BatchLimit = 128

type Event struct {
	Sequence  uint64          `json:"sequence"`
	Kind      string          `json:"kind"`
	RequestID string          `json:"request_id"`
	Identity  string          `json:"identity,omitempty"`
	AccountID string          `json:"account_id,omitempty"`
	At        time.Time       `json:"at"`
	AuthIndex string          `json:"auth_index,omitempty"`
	Binding   string          `json:"binding,omitempty"`
	Source    string          `json:"source,omitempty"`
	Headers   http.Header     `json:"headers,omitempty"`
	Quota     json.RawMessage `json:"quota,omitempty"`
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
