package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"codex-window-refresh/internal/v2/store"
)

func (c *CPA) wakeAccount(ctx context.Context, account store.Account) error {
	if account.Status == "disabled" {
		return errors.New("account_disabled")
	}
	if !store.ValidID(account.ID) || account.Status != "available" {
		return errors.New("account_identity_unavailable")
	}
	if strings.TrimSpace(account.AuthIndex) == "" || strings.TrimSpace(account.Binding) == "" {
		return errors.New("wake_binding_missing")
	}
	entries, counts, indexes, err := c.generationInventory(ctx)
	if err != nil {
		return errors.New("account_identity_unavailable")
	}
	if counts[account.ID] == 0 {
		return errors.New("account_not_found")
	}
	if counts[account.ID] != 1 {
		return errors.New("wake_account_ambiguous")
	}
	var selected authEntry
	for _, entry := range entries {
		if entry.ID == account.ID {
			selected = entry
			break
		}
	}
	if strings.TrimSpace(selected.Index) == "" {
		return errors.New("wake_binding_missing")
	}
	if indexes[selected.Index] != 1 {
		return errors.New("wake_account_ambiguous")
	}
	if selected.RuntimeOnly {
		return errors.New("account_identity_unavailable")
	}
	if generationProvider(selected) == "" {
		return errors.New("account_identity_unavailable")
	}
	if selected.Index != account.AuthIndex || generationProvider(selected) != generationSnapshotProvider(account) {
		return errors.New("account_binding_changed")
	}
	if err = wakeAccountStatus(selected, false); err != nil {
		return err
	}
	var runtime struct {
		Auth authEntry `json:"auth"`
	}
	if c.Host.Call(ctx, "host.auth.get_runtime", map[string]string{"auth_index": account.AuthIndex}, &runtime) != nil {
		return errors.New("account_identity_unavailable")
	}
	if runtime.Auth.ID == "" || runtime.Auth.Index == "" || generationProvider(runtime.Auth) == "" {
		return errors.New("account_identity_unavailable")
	}
	if runtime.Auth.ID != account.ID || runtime.Auth.Index != account.AuthIndex || generationProvider(runtime.Auth) != generationSnapshotProvider(account) {
		return errors.New("account_binding_changed")
	}
	if runtime.Auth.RuntimeOnly {
		return errors.New("account_identity_unavailable")
	}
	if err = wakeAccountStatus(runtime.Auth, true); err != nil {
		return err
	}
	var raw struct {
		Index string          `json:"auth_index"`
		JSON  json.RawMessage `json:"json"`
	}
	defer func() { clear(raw.JSON) }()
	if c.Host.Call(ctx, "host.auth.get", map[string]string{"auth_index": account.AuthIndex}, &raw) != nil {
		return errors.New("account_identity_unavailable")
	}
	var identity struct {
		Type     string `json:"type"`
		Upstream string `json:"account_id"`
	}
	if len(raw.JSON) > 1<<20 || json.Unmarshal(raw.JSON, &identity) != nil || identity.Type != "codex" {
		return errors.New("account_identity_unavailable")
	}
	if strings.TrimSpace(raw.Index) == "" || strings.TrimSpace(identity.Upstream) == "" {
		return errors.New("wake_binding_missing")
	}
	if raw.Index != account.AuthIndex || bindingID(account, identity.Upstream) != account.Binding {
		return errors.New("account_binding_changed")
	}
	return nil
}

func wakeAccountStatus(entry authEntry, runtime bool) error {
	status := generationStatus(entry)
	if runtime {
		status = accountStatus(entry)
	}
	switch status {
	case "disabled":
		return errors.New("account_disabled")
	case "unavailable":
		if entry.Unavailable || entry.Status == "unavailable" {
			return errors.New("wake_host_unavailable")
		}
		return errors.New("account_identity_unavailable")
	}
	return nil
}
