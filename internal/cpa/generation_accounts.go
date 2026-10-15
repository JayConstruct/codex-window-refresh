package cpa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"codex-window-refresh/internal/core"
)

// GenerationAccounts returns the last local inventory. Reading the catalog
// never discovers models or queries quota upstream.
func (h *Host) GenerationAccounts() map[core.AccountID]core.GenerationAccount {
	h.mu.RLock()
	defer h.mu.RUnlock()
	accounts := make(map[core.AccountID]core.GenerationAccount, len(h.generationAccounts))
	for id, account := range h.generationAccounts {
		accounts[id] = account
	}
	return accounts
}

func (h *Host) readGenerationAccount(ctx context.Context, id core.AccountID, index string) (core.GenerationAccount, error) {
	if h.callback == nil || strings.TrimSpace(index) == "" {
		return core.GenerationAccount{}, ErrBindingUnavailable
	}
	var runtime authRuntime
	if err := h.callback.Call(ctx, "host.auth.get_runtime", authRequest{index}, &runtime); err != nil {
		if contextErr := inventoryContextError(ctx, err); contextErr != nil {
			return core.GenerationAccount{}, contextErr
		}
		return core.GenerationAccount{}, ErrHostCallback
	}
	auth := runtime.Auth
	if auth.ID != string(id) || auth.AuthIndex != index || provider(auth) != "antigravity" {
		return core.GenerationAccount{}, ErrBindingChanged
	}
	if strings.TrimSpace(auth.Email) == "" {
		return core.GenerationAccount{}, ErrBindingUnavailable
	}
	// OAuth refreshes do not change this identity; neither email nor project ID
	// is retained in the catalog or durable state.
	digest := sha256.Sum256([]byte("antigravity-generation-v1\x00" + string(id) + "\x00" + strings.TrimSpace(auth.Email) + "\x00" + strings.TrimSpace(auth.ProjectID)))
	status := availability(auth)
	return core.GenerationAccount{ID: id, Provider: "antigravity", Generation: core.CredentialGeneration(hex.EncodeToString(digest[:])), Enabled: status == core.AccountAvailable, Availability: status}, nil
}

func (h *Host) readGenerationAccounts(ctx context.Context, entries []authEntry, counts map[core.AccountID]int) (map[core.AccountID]core.GenerationAccount, map[core.AccountID]string, error) {
	accounts, bindings := map[core.AccountID]core.GenerationAccount{}, map[core.AccountID]string{}
	for _, entry := range entries {
		if provider(entry) != "antigravity" || strings.TrimSpace(entry.ID) == "" {
			continue
		}
		id := core.AccountID(entry.ID)
		if counts[id] != 1 {
			continue
		}
		account, err := h.readGenerationAccount(ctx, id, entry.AuthIndex)
		if contextErr := inventoryContextError(ctx, err); contextErr != nil {
			return nil, nil, contextErr
		}
		if err != nil {
			continue
		}
		accounts[id], bindings[id] = account, entry.AuthIndex
	}
	return accounts, bindings, nil
}

func (h *Host) validateGenerationAccount(ctx context.Context, id core.AccountID, generation core.CredentialGeneration) error {
	h.mu.RLock()
	index, exists := h.generationBindings[id]
	previous := h.generationAccounts[id]
	h.mu.RUnlock()
	if !exists || previous.Generation != generation {
		return ErrBindingUnavailable
	}
	current, err := h.readGenerationAccount(ctx, id, index)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.Generation != generation {
		return ErrBindingChanged
	}
	if !current.Enabled {
		return ErrBindingUnavailable
	}
	return nil
}
