package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/store"
)

func generationProvider(entry authEntry) string {
	if entry.Provider != "" {
		return entry.Provider
	}
	return entry.Type
}

func generationSnapshotProvider(account store.Account) string {
	if account.Provider == "" {
		return "codex"
	}
	return account.Provider
}

func generationStatus(entry authEntry) string {
	if entry.Disabled || entry.Status == "disabled" {
		return "disabled"
	}
	if entry.Unavailable || entry.Status == "unavailable" {
		return "unavailable"
	}
	return "available"
}

func (c *CPA) GenerationProviders() []string {
	providers := []string{}
	if c.Host == nil {
		return providers
	}
	if capability, ok := c.Host.(interface{ SupportsPromptGeneration() bool }); ok && !capability.SupportsPromptGeneration() {
		return providers
	}
	if capability, ok := c.Host.(interface{ SupportedGenerationProviders() []string }); ok {
		advertised := capability.SupportedGenerationProviders()
		for _, provider := range []string{"codex", "antigravity"} {
			for _, supported := range advertised {
				if supported == provider {
					providers = append(providers, provider)
					break
				}
			}
		}
		return providers
	}
	return []string{"codex"}
}

func (c *CPA) generationInventory(ctx context.Context) ([]authEntry, map[string]int, map[string]int, error) {
	if c.Host == nil {
		return nil, nil, nil, errors.New("account_identity_unavailable")
	}
	var list struct {
		Files []authEntry `json:"files"`
	}
	if err := c.Host.Call(ctx, "host.auth.list", struct{}{}, &list); err != nil {
		return nil, nil, nil, errors.New("account_identity_unavailable")
	}
	if len(list.Files) > 10000 {
		return nil, nil, nil, errors.New("inventory_too_large")
	}
	counts, indexes := map[string]int{}, map[string]int{}
	for _, entry := range list.Files {
		counts[entry.ID]++
		if entry.Index != "" {
			indexes[entry.Index]++
		}
	}
	return list.Files, counts, indexes, nil
}

func (c *CPA) GenerationAccounts(ctx context.Context) ([]store.Account, error) {
	entries, counts, indexes, err := c.generationInventory(ctx)
	if err != nil {
		return nil, err
	}
	accounts, seen := []store.Account{}, map[string]bool{}
	for _, entry := range entries {
		provider := generationProvider(entry)
		if (provider != "codex" && provider != "antigravity") || !store.ValidID(entry.ID) || seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		account := store.Account{ID: entry.ID, Provider: provider, AuthIndex: entry.Index, Status: generationStatus(entry)}
		if counts[entry.ID] != 1 || entry.Index == "" || indexes[entry.Index] != 1 || entry.RuntimeOnly {
			account.Status, account.AuthIndex = "binding_unavailable", ""
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (c *CPA) ResolveGenerationAccount(ctx context.Context, id string) (store.Account, error) {
	if !store.ValidID(id) {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	entries, counts, indexes, err := c.generationInventory(ctx)
	if err != nil {
		return store.Account{}, err
	}
	if counts[id] == 0 {
		return store.Account{}, errors.New("account_not_found")
	}
	if counts[id] != 1 {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	var selected authEntry
	for _, entry := range entries {
		if entry.ID == id {
			selected = entry
			break
		}
	}
	provider := generationProvider(selected)
	if (provider != "codex" && provider != "antigravity") || selected.RuntimeOnly || strings.TrimSpace(selected.Index) == "" || indexes[selected.Index] != 1 {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	account := store.Account{ID: selected.ID, Provider: provider, AuthIndex: selected.Index, Status: generationStatus(selected)}
	if account.Status == "disabled" {
		return store.Account{}, errors.New("account_disabled")
	}
	if account.Status != "available" {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	if provider == "codex" {
		credential, err := c.readBindingFor(ctx, account, true)
		if err != nil {
			return store.Account{}, generationBindingError(err)
		}
		account.Binding = bindingID(account, credential.Upstream)
		return account, nil
	}
	var runtime struct {
		Auth authEntry `json:"auth"`
	}
	if c.Host.Call(ctx, "host.auth.get_runtime", map[string]string{"auth_index": account.AuthIndex}, &runtime) != nil {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	entry := runtime.Auth
	if entry.ID != account.ID || entry.Index != account.AuthIndex || generationProvider(entry) != provider || entry.RuntimeOnly {
		return store.Account{}, errors.New("account_binding_changed")
	}
	if generationStatus(entry) == "disabled" {
		return store.Account{}, errors.New("account_disabled")
	}
	if generationStatus(entry) != "available" {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	if !generationMetadataValid(entry.Email, 4096) || !generationMetadataValid(entry.ProjectID, 4096) || strings.TrimSpace(entry.Email) == "" {
		return store.Account{}, errors.New("account_identity_unavailable")
	}
	digest := sha256.Sum256([]byte("antigravity-generation-v1\x00" + account.ID + "\x00" + account.AuthIndex + "\x00" + strings.TrimSpace(entry.Email) + "\x00" + strings.TrimSpace(entry.ProjectID)))
	account.Binding = hex.EncodeToString(digest[:])
	return account, nil
}

func generationMetadataValid(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func generationBindingError(err error) error {
	switch err.Error() {
	case "account_binding_changed", "account_disabled", "account_identity_unavailable":
		return errors.New(err.Error())
	}
	return errors.New("account_identity_unavailable")
}
