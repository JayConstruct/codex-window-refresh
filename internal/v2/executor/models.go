package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/store"
)

// AccountModelCatalog contains only the registered model names for one verified
// host account. It never exposes host indices or credential JSON to the panel.
type AccountModelCatalog struct {
	AccountID string   `json:"account_id"`
	Provider  string   `json:"provider"`
	Models    []string `json:"models"`
}

func (c *CPA) AccountModelsAvailable() bool { return c.Host != nil }

func (c *CPA) AccountModels(ctx context.Context, id string) (AccountModelCatalog, error) {
	if !validCatalogAccountID(id) || c.Host == nil {
		return AccountModelCatalog{}, errors.New("model_catalog_account_invalid")
	}
	instanceHost, pinInstance := c.Host.(interface{ Instance() string })
	instance := ""
	if pinInstance {
		instance = instanceHost.Instance()
	}
	accounts, err := c.GenerationAccounts(ctx)
	if err != nil {
		return AccountModelCatalog{}, catalogError(ctx, err)
	}
	if pinInstance && instance != "" && instanceHost.Instance() != instance {
		return AccountModelCatalog{}, errors.New("model_catalog_changed")
	}
	for _, account := range accounts {
		if account.ID == id {
			return c.modelsForAccount(ctx, account)
		}
	}
	return AccountModelCatalog{}, errors.New("model_catalog_account_invalid")
}

func validCatalogModel(model string) bool {
	return model != "" && len(model) <= 200 && utf8.ValidString(model) && strings.IndexFunc(model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func validCatalogAccountID(id string) bool {
	return store.ValidID(id) && strings.TrimSpace(id) != "" && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func catalogError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && (err.Error() == "model_catalog_unsupported" || err.Error() == "method_not_allowed") {
		return errors.New("model_catalog_unsupported")
	}
	return errors.New("model_catalog_unavailable")
}

func (c *CPA) modelsForAccount(ctx context.Context, account store.Account) (AccountModelCatalog, error) {
	invalid := func() (AccountModelCatalog, error) {
		return AccountModelCatalog{}, errors.New("model_catalog_account_invalid")
	}
	provider := generationSnapshotProvider(account)
	if c.Host == nil || !validCatalogAccountID(account.ID) || !validCatalogAccountID(account.AuthIndex) || account.Status != "available" || (provider != "codex" && provider != "antigravity") {
		return invalid()
	}
	instanceHost, pinInstance := c.Host.(interface{ Instance() string })
	instance := ""
	if pinInstance {
		instance = instanceHost.Instance()
		if instance == "" {
			return AccountModelCatalog{}, errors.New("model_catalog_unavailable")
		}
	}
	verify := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pinInstance && instanceHost.Instance() != instance {
			return errors.New("model_catalog_changed")
		}
		var runtime struct {
			Auth authEntry `json:"auth"`
		}
		if err := c.Host.Call(ctx, "host.auth.get_runtime", map[string]string{"auth_index": account.AuthIndex}, &runtime); err != nil {
			return catalogError(ctx, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entry := runtime.Auth
		if entry.ID != account.ID || entry.Index != account.AuthIndex || generationProvider(entry) != provider || entry.RuntimeOnly || generationStatus(entry) != "available" || pinInstance && instanceHost.Instance() != instance {
			return errors.New("model_catalog_changed")
		}
		return nil
	}
	if err := verify(); err != nil {
		return AccountModelCatalog{}, err
	}
	var raw json.RawMessage
	if err := c.Host.Call(ctx, "host.auth.models", map[string]string{"auth_index": account.AuthIndex}, &raw); err != nil {
		return AccountModelCatalog{}, catalogError(ctx, err)
	}
	var response struct {
		Credentials []struct {
			ID       string   `json:"id"`
			Index    string   `json:"auth_index"`
			Provider string   `json:"provider"`
			Models   []string `json:"models"`
		} `json:"credentials"`
	}
	if len(raw) > 512<<10 || json.Unmarshal(raw, &response) != nil || len(response.Credentials) != 1 {
		return AccountModelCatalog{}, errors.New("model_catalog_unavailable")
	}
	credential := response.Credentials[0]
	if credential.ID != account.ID || credential.Index != account.AuthIndex || credential.Provider != provider {
		return AccountModelCatalog{}, errors.New("model_catalog_changed")
	}
	if credential.Models == nil || len(credential.Models) > 2048 {
		return AccountModelCatalog{}, errors.New("model_catalog_unavailable")
	}
	models, seen := []string{}, map[string]bool{}
	for _, model := range credential.Models {
		if !validCatalogModel(model) {
			return AccountModelCatalog{}, errors.New("model_catalog_unavailable")
		}
		if !seen[model] {
			models = append(models, model)
			seen[model] = true
		}
	}
	if err := verify(); err != nil {
		return AccountModelCatalog{}, err
	}
	return AccountModelCatalog{AccountID: account.ID, Provider: provider, Models: models}, nil
}
