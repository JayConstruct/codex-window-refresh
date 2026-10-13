package cpa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"codex-window-refresh/internal/core"
)

var ErrMissingBridge = errors.New("quota_management_bridge_missing")
var ErrBindingUnavailable = errors.New("account_identity_unavailable")
var ErrBindingChanged = errors.New("account_binding_changed")
var ErrHostCallback = errors.New("host_callback_failed")

func (h *Host) Capabilities(context.Context) (core.HostCapabilities, error) {
	ready := h.callback != nil && h.reader != nil
	if reader, ok := h.reader.(interface{ Available() bool }); ok {
		ready = ready && reader.Available()
	}
	return core.HostCapabilities{StableAccountIdentity: h.callback != nil, ExactAccountExecution: h.callback != nil, QuotaAdapterReady: ready, ActivityIdentityAtStart: h.callback != nil, ActivityIdentityAtCompletion: h.callback != nil, ForegroundGuard: false}, nil
}

func (h *Host) QuotaConnectionStatus() string {
	if reader, ok := h.reader.(interface{ ConnectionStatus() string }); ok {
		return reader.ConnectionStatus()
	}
	return ""
}

func (h *Host) CheckQuotaConnection(ctx context.Context) error {
	if reader, ok := h.reader.(interface{ CheckConnection(context.Context) error }); ok {
		return reader.CheckConnection(ctx)
	}
	return ErrMissingBridge
}
func provider(e authEntry) string {
	if e.Provider != "" {
		return e.Provider
	}
	return e.Type
}
func enabled(e authEntry) bool { return !e.Disabled && !e.Unavailable && e.Status != "disabled" }
func generation(hostID, upstream string) core.CredentialGeneration {
	sum := sha256.Sum256([]byte("codex-account-v1\x00" + hostID + "\x00" + upstream))
	return core.CredentialGeneration(hex.EncodeToString(sum[:]))
}
func (h *Host) readBinding(ctx context.Context, id core.AccountID, index string) (AuthBinding, bool, error) {
	if h.callback == nil || id == "" || index == "" {
		return AuthBinding{}, false, ErrBindingUnavailable
	}
	var runtime authRuntime
	if err := h.callback.Call(ctx, "host.auth.get_runtime", authRequest{index}, &runtime); err != nil {
		return AuthBinding{}, false, ErrHostCallback
	}
	if runtime.Auth.ID != string(id) || runtime.Auth.AuthIndex != index || provider(runtime.Auth) != "codex" {
		return AuthBinding{}, false, ErrBindingChanged
	}
	if runtime.Auth.RuntimeOnly {
		return AuthBinding{}, false, ErrBindingUnavailable
	}
	var raw authGet
	defer func() { clear(raw.JSON) }()
	if err := h.callback.Call(ctx, "host.auth.get", authRequest{index}, &raw); err != nil {
		return AuthBinding{}, false, ErrBindingUnavailable
	}
	// Decode only the allowlisted account_id; secrets are never retained or logged.
	var metadata struct {
		AccountID string `json:"account_id"`
		Type      string `json:"type"`
	}
	if raw.AuthIndex != index || json.Unmarshal(raw.JSON, &metadata) != nil || strings.TrimSpace(metadata.AccountID) == "" || metadata.Type != "codex" {
		return AuthBinding{}, false, ErrBindingUnavailable
	}
	return AuthBinding{AccountID: id, AuthIndex: index, UpstreamAccountID: metadata.AccountID, Generation: generation(string(id), metadata.AccountID)}, enabled(runtime.Auth), nil
}
func (h *Host) Accounts(ctx context.Context) ([]core.HostAccount, error) {
	if h.callback == nil {
		return nil, ErrHostCallback
	}
	var list authList
	if err := h.callback.Call(ctx, "host.auth.list", struct{}{}, &list); err != nil {
		return nil, ErrHostCallback
	}
	bindings := map[core.AccountID]AuthBinding{}
	accounts := map[core.AccountID]core.HostAccount{}
	result := []core.HostAccount{}
	for _, entry := range list.Files {
		if provider(entry) != "codex" {
			continue
		}
		id := core.AccountID(entry.ID)
		if id == "" || entry.AuthIndex == "" {
			return nil, ErrBindingUnavailable
		}
		if _, ok := accounts[id]; ok {
			return nil, ErrBindingUnavailable
		}
		b, active, err := h.readBinding(ctx, id, entry.AuthIndex)
		if err != nil {
			return nil, err
		}
		a := core.HostAccount{ID: id, Generation: b.Generation, Enabled: active}
		bindings[id] = b
		accounts[id] = a
		result = append(result, a)
	}
	h.mu.Lock()
	h.bindings = bindings
	h.accounts = accounts
	h.mu.Unlock()
	return result, nil
}
func (h *Host) validate(ctx context.Context, id core.AccountID, gen core.CredentialGeneration) (AuthBinding, error) {
	return h.validateBinding(ctx, id, gen, true)
}

func (h *Host) validateBinding(ctx context.Context, id core.AccountID, gen core.CredentialGeneration, requireActive bool) (AuthBinding, error) {
	h.mu.RLock()
	old, ok := h.bindings[id]
	h.mu.RUnlock()
	if !ok {
		return AuthBinding{}, ErrBindingUnavailable
	}
	b, active, err := h.readBinding(ctx, id, old.AuthIndex)
	if err != nil {
		return AuthBinding{}, err
	}
	if b.Generation != gen || old.Generation != gen {
		return AuthBinding{}, ErrBindingChanged
	}
	if requireActive && !active {
		return AuthBinding{}, ErrBindingUnavailable
	}
	return b, nil
}
func (h *Host) ObserveQuota(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
	if h.reader == nil {
		return core.QuotaObservation{}, ErrMissingBridge
	}
	b, err := h.validateBinding(ctx, read.AccountID, read.Generation, false)
	if err != nil {
		return core.QuotaObservation{}, err
	}
	raw, err := h.reader.Read(ctx, b)
	defer clear(raw)
	if err != nil {
		return core.QuotaObservation{}, err
	}
	if _, err = h.validateBinding(ctx, read.AccountID, read.Generation, false); err != nil {
		return core.QuotaObservation{}, err
	}
	h.mu.RLock()
	baseline := h.baselines[read.AccountID]
	h.mu.RUnlock()
	observation, err := ParseQuota(raw, read, time.Now().UTC(), core.ObservationQuery, baseline)
	if err != nil {
		return core.QuotaObservation{}, quotaError(ErrQuotaMalformed, "parse", http.StatusOK)
	}
	return observation, nil
}
