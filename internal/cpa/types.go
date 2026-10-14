// Package cpa adapts the JSON callback contract of a pinned CPA v8 host.
// It does not import the upstream SDK or register native plugin callbacks.
package cpa

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"codex-window-refresh/internal/core"
)

type Callback interface {
	Call(context.Context, string, any, any) error
}
type QuotaReader interface {
	Read(context.Context, AuthBinding) ([]byte, error)
}

// AuthBinding is memory-only. Generation hashes actual upstream identity, not
// OAuth token text, refreshed timestamps, names, or email addresses.
type AuthBinding struct {
	AccountID         core.AccountID
	AuthIndex         string
	UpstreamAccountID string
	Generation        core.CredentialGeneration
}
type Host struct {
	callback             Callback
	reader               QuotaReader
	mu                   sync.RWMutex
	bindings             map[core.AccountID]AuthBinding
	accounts             map[core.AccountID]core.HostAccount
	inventoryDiagnostics map[core.AccountID]string
	baselines            map[core.AccountID]core.AccountState
}

func NewHost(callback Callback, reader QuotaReader) *Host {
	return &Host{callback: callback, reader: reader, bindings: map[core.AccountID]AuthBinding{}, accounts: map[core.AccountID]core.HostAccount{}, baselines: map[core.AccountID]core.AccountState{}}
}
func (h *Host) SetBaseline(state core.CoreState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.baselines = make(map[core.AccountID]core.AccountState, len(state.Accounts))
	for id, a := range state.Accounts {
		a.Task = nil
		h.baselines[id] = a
	}
}
func (h *Host) ActivityAccounts() map[core.AccountID]core.HostAccount {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make(map[core.AccountID]core.HostAccount, len(h.accounts))
	for id, a := range h.accounts {
		// An inventory placeholder has no validated identity and cannot be used
		// to attribute activity or response evidence to a credential.
		if _, ok := h.bindings[id]; !ok {
			continue
		}
		result[id] = a
	}
	return result
}

// AccountInventoryDiagnostics returns only stable, allowlisted error codes.
// A blank ID identifies an entry that cannot safely be represented as an account.
func (h *Host) AccountInventoryDiagnostics() map[core.AccountID]string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make(map[core.AccountID]string, len(h.inventoryDiagnostics))
	for id, code := range h.inventoryDiagnostics {
		result[id] = code
	}
	return result
}

type authEntry struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Provider    string `json:"provider"`
	Type        string `json:"type"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Status      string `json:"status"`
	RuntimeOnly bool   `json:"runtime_only"`
}
type authList struct {
	Files []authEntry `json:"files"`
}
type authRequest struct {
	AuthIndex string `json:"auth_index"`
}
type authRuntime struct {
	Auth authEntry `json:"auth"`
}
type authGet struct {
	AuthIndex string          `json:"auth_index"`
	JSON      json.RawMessage `json:"json"`
}
type modelRequest struct {
	EntryProtocol  string              `json:"entry_protocol"`
	ExitProtocol   string              `json:"exit_protocol"`
	Model          string              `json:"model"`
	Stream         bool                `json:"stream"`
	Body           []byte              `json:"body"`
	Headers        http.Header         `json:"headers"`
	Query          map[string][]string `json:"query"`
	Alt            string              `json:"alt"`
	ForcedProvider string              `json:"forced_provider"`
	AuthID         string              `json:"auth_id"`
}
type streamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type streamRequest struct {
	StreamID string `json:"stream_id"`
}
type streamRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}
type requestMetadata struct {
	RequestID   string
	Metadata    map[string]json.RawMessage
	StartedAt   time.Time
	CompletedAt time.Time
	Headers     http.Header
	Body        []byte
}
