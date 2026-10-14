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
		if err := inventoryContextError(ctx, err); err != nil {
			return AuthBinding{}, false, err
		}
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
		if err := inventoryContextError(ctx, err); err != nil {
			return AuthBinding{}, false, err
		}
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

func inventoryContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func unavailableGeneration(id core.AccountID) core.CredentialGeneration {
	// This is deliberately outside the validated credential namespace. Replacing
	// a known binding with this placeholder invalidates all prior quota evidence.
	sum := sha256.Sum256([]byte("codex-account-unavailable-v1\x00" + string(id)))
	return core.CredentialGeneration(hex.EncodeToString(sum[:]))
}

func (h *Host) Accounts(ctx context.Context) ([]core.HostAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.callback == nil {
		return nil, ErrHostCallback
	}
	var list authList
	if err := h.callback.Call(ctx, "host.auth.list", struct{}{}, &list); err != nil {
		if err := inventoryContextError(ctx, err); err != nil {
			return nil, err
		}
		return nil, ErrHostCallback
	}
	bindings := map[core.AccountID]AuthBinding{}
	accounts := map[core.AccountID]core.HostAccount{}
	diagnostics := map[core.AccountID]string{}
	result := []core.HostAccount{}
	counts := map[core.AccountID]int{}
	for _, entry := range list.Files {
		if provider(entry) == "codex" {
			counts[core.AccountID(entry.ID)]++
		}
	}
	for _, entry := range list.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if provider(entry) != "codex" {
			continue
		}
		id := core.AccountID(entry.ID)
		if strings.TrimSpace(string(id)) == "" {
			diagnostics[""] = ErrBindingUnavailable.Error()
			continue
		}
		if _, ok := accounts[id]; ok {
			continue
		}
		a := core.HostAccount{ID: id, Generation: unavailableGeneration(id)}
		if counts[id] > 1 {
			diagnostics[id] = "account_identity_ambiguous"
		} else if strings.TrimSpace(entry.AuthIndex) == "" || entry.RuntimeOnly {
			diagnostics[id] = ErrBindingUnavailable.Error()
		} else {
			b, active, err := h.readBinding(ctx, id, entry.AuthIndex)
			if contextErr := inventoryContextError(ctx, err); contextErr != nil {
				return nil, contextErr
			}
			if err != nil {
				diagnostics[id] = err.Error()
			} else {
				a.Generation, a.Enabled = b.Generation, active
				bindings[id] = b
			}
		}
		accounts[id] = a
		result = append(result, a)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.bindings = bindings
	h.accounts = accounts
	h.inventoryDiagnostics = diagnostics
	for id, baseline := range h.baselines {
		binding, ok := bindings[id]
		if !ok || baseline.Generation != binding.Generation {
			delete(h.baselines, id)
		}
	}
	h.mu.Unlock()
	return result, nil
}

// Account refreshes one previously validated identity without scanning other
// credentials. Unknown and ambiguous identities require a full inventory first.
func (h *Host) Account(ctx context.Context, id core.AccountID) (core.HostAccount, error) {
	if err := ctx.Err(); err != nil {
		return core.HostAccount{}, err
	}
	h.mu.RLock()
	old, bound := h.bindings[id]
	a, known := h.accounts[id]
	h.mu.RUnlock()
	if !bound {
		if known {
			return a, nil
		}
		return core.HostAccount{}, ErrBindingUnavailable
	}
	b, active, err := h.readBinding(ctx, id, old.AuthIndex)
	if contextErr := inventoryContextError(ctx, err); contextErr != nil {
		return core.HostAccount{}, contextErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return core.HostAccount{}, contextErr
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// A concurrent inventory may have removed or replaced this account while
	// its callback was running. Never restore that obsolete binding.
	if current, ok := h.bindings[id]; !ok || current != old {
		return core.HostAccount{}, ErrBindingChanged
	}
	if err != nil {
		a = core.HostAccount{ID: id, Generation: unavailableGeneration(id)}
		delete(h.bindings, id)
		if h.inventoryDiagnostics == nil {
			h.inventoryDiagnostics = map[core.AccountID]string{}
		}
		h.inventoryDiagnostics[id] = err.Error()
	} else {
		a = core.HostAccount{ID: id, Generation: b.Generation, Enabled: active}
		h.bindings[id] = b
		delete(h.inventoryDiagnostics, id)
	}
	h.accounts[id] = a
	if baseline, ok := h.baselines[id]; ok && (err != nil || baseline.Generation != a.Generation) {
		delete(h.baselines, id)
	}
	return a, nil
}

// Close releases resources owned by the quota reader after service shutdown.
func (h *Host) Close() error {
	if closer, ok := h.reader.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
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
