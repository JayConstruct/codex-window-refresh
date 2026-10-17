package executor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"codex-window-refresh/internal/v2/store"
)

type generationProviderHost struct {
	*generationHost
	providers []string
}

func (host *generationProviderHost) SupportedGenerationProviders() []string {
	return append([]string(nil), host.providers...)
}

func antigravityFixture(t *testing.T) (*CPA, *generationHost, store.Account) {
	t.Helper()
	cpa, host, _ := generationFixture()
	host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"antigravity"}]}`
	host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"antigravity","email":" owner@example.test ","project_id":" project-fixture ","access_token":"secret-token","updated_at":"first-update"}}`
	cpa.Host = &generationProviderHost{host, []string{"codex", "antigravity"}}
	account, err := cpa.ResolveGenerationAccount(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	return cpa, host, account
}

func TestGenerationProviderCapabilitiesAreCachedAndConservative(t *testing.T) {
	cpa, host, _ := generationFixture()
	if got := cpa.GenerationProviders(); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("legacy providers=%v", got)
	}
	cpa.Host = &generationProviderHost{host, []string{"antigravity", "unknown", "codex", "codex"}}
	providers := cpa.GenerationProviders()
	if !reflect.DeepEqual(providers, []string{"codex", "antigravity"}) || !cpa.GenerationAvailable() {
		t.Fatalf("providers=%v", providers)
	}
	providers[0] = "mutated"
	if got := cpa.GenerationProviders(); got[0] != "codex" {
		t.Fatal("mutable capability escaped")
	}
	cpa.Host = &generationProviderHost{host, nil}
	if len(cpa.GenerationProviders()) != 0 || cpa.GenerationAvailable() {
		t.Fatal("empty provider negotiation allowed generation")
	}
	cpa.Host = &generationUnsupportedHost{host}
	if len(cpa.GenerationProviders()) != 0 || cpa.GenerationAvailable() {
		t.Fatal("unsupported host advertises providers")
	}
	if len((&CPA{}).GenerationProviders()) != 0 || host.lists != 0 || host.runtimes != 0 || host.credentials != 0 || host.executes != 0 {
		t.Fatal("capability getters made callbacks")
	}
}

func TestGenerationCatalogIsReadOnlyAndSeparateFromQuotaAccounts(t *testing.T) {
	cpa, host, _ := generationFixture()
	host.inventory = `{"files":[
		{"id":"codex","auth_index":"c","provider":"codex"},
		{"id":"antig","auth_index":"g","type":"antigravity","email":"private@example.test","project_id":"private-project"},
		{"id":"disabled","auth_index":"d","provider":"antigravity","status":"disabled"},
		{"id":"unavailable","auth_index":"u","provider":"antigravity","status":"unavailable"},
		{"id":"runtime-only","auth_index":"r","provider":"antigravity","runtime_only":true},
		{"id":"missing-index","provider":"antigravity"},
		{"id":"duplicate-id","auth_index":"one","provider":"antigravity"},
		{"id":"duplicate-id","auth_index":"two","provider":"other"},
		{"id":"duplicate-index","auth_index":"shared","provider":"antigravity"},
		{"id":"other","auth_index":"shared","provider":"other"},
		{"id":"unsupported","auth_index":"unsupported","provider":"other"}
	]}`
	accounts, err := cpa.GenerationAccounts(context.Background())
	if err != nil || len(accounts) != 8 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	wanted := []string{"available", "available", "disabled", "unavailable", "binding_unavailable", "binding_unavailable", "binding_unavailable", "binding_unavailable"}
	for index, account := range accounts {
		if account.Status != wanted[index] || account.Binding != "" || (index != 0 && account.Provider != "antigravity") {
			t.Fatalf("unexpected catalog account=%+v", account)
		}
		if account.Status == "binding_unavailable" && account.AuthIndex != "" {
			t.Fatalf("ambiguous catalog exposed index: %+v", account)
		}
	}
	raw, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private@", "private-project", "auth_index", "email", "project_id", "token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("catalog leaked %q: %s", secret, raw)
		}
	}
	if host.lists != 1 || host.runtimes != 0 || host.credentials != 0 || host.executes != 0 {
		t.Fatalf("catalog called runtime/credentials/model: %+v", host)
	}
	quotaAccounts, err := cpa.Accounts(context.Background())
	if err != nil || len(quotaAccounts) != 1 || quotaAccounts[0].ID != "codex" || quotaAccounts[0].Provider != "" {
		t.Fatalf("quota inventory changed=%+v err=%v", quotaAccounts, err)
	}
}

func TestGenerationCatalogRejectsOversizeInventoryAndSanitizesErrors(t *testing.T) {
	cpa, host, _ := generationFixture()
	host.inventory = `{"files":[` + strings.Repeat(`{"id":"a","provider":"codex"},`, 10000) + `{"id":"a","provider":"codex"}]}`
	if _, err := cpa.GenerationAccounts(context.Background()); err == nil || err.Error() != "inventory_too_large" {
		t.Fatalf("inventory bound err=%v", err)
	}
	host.authErr = errors.New("private credential error")
	if _, err := cpa.GenerationAccounts(context.Background()); err == nil || err.Error() != "account_identity_unavailable" {
		t.Fatalf("public err=%v", err)
	}
	if _, err := (&CPA{}).GenerationAccounts(context.Background()); err == nil || err.Error() != "account_identity_unavailable" {
		t.Fatalf("missing host err=%v", err)
	}
}

func TestResolveGenerationAccountRejectsUnsafeInventory(t *testing.T) {
	tests := []struct {
		name      string
		inventory string
		code      string
	}{
		{"missing", `{"files":[]}`, "account_not_found"},
		{"cross-provider duplicate id", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity"},{"id":"a","auth_index":"other","provider":"codex"}]}`, "account_identity_unavailable"},
		{"unsupported-provider duplicate id", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity"},{"id":"a","auth_index":"other","provider":"other"}]}`, "account_identity_unavailable"},
		{"cross-provider duplicate index", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity"},{"id":"b","auth_index":"i","provider":"other"}]}`, "account_identity_unavailable"},
		{"unsupported", `{"files":[{"id":"a","auth_index":"i","provider":"other"}]}`, "account_identity_unavailable"},
		{"runtime only", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity","runtime_only":true}]}`, "account_identity_unavailable"},
		{"missing index", `{"files":[{"id":"a","provider":"antigravity"}]}`, "account_identity_unavailable"},
		{"blank index", `{"files":[{"id":"a","auth_index":"  ","provider":"antigravity"}]}`, "account_identity_unavailable"},
		{"disabled", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity","disabled":true}]}`, "account_disabled"},
		{"disabled status", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity","status":"disabled"}]}`, "account_disabled"},
		{"unavailable", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity","unavailable":true}]}`, "account_identity_unavailable"},
		{"unavailable status", `{"files":[{"id":"a","auth_index":"i","provider":"antigravity","status":"unavailable"}]}`, "account_identity_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, _ := generationFixture()
			host.inventory = test.inventory
			account, err := cpa.ResolveGenerationAccount(context.Background(), "a")
			if err == nil || err.Error() != test.code || account.ID != "" || host.runtimes != 0 || host.credentials != 0 || host.executes != 0 {
				t.Fatalf("account=%+v err=%v runtime=%d get=%d model=%d", account, err, host.runtimes, host.credentials, host.executes)
			}
		})
	}
}

func TestAntigravityResolveRuntimeIdentityAndPrivacy(t *testing.T) {
	cpa, host, account := antigravityFixture(t)
	if account.Provider != "antigravity" || account.Binding == "" || account.Status != "available" || host.credentials != 0 || host.runtimes != 1 || host.executes != 0 {
		t.Fatalf("account=%+v runtime=%d credentials=%d", account, host.runtimes, host.credentials)
	}
	var runtime map[string]map[string]any
	if err := json.Unmarshal([]byte(host.runtime), &runtime); err != nil {
		t.Fatal(err)
	}
	runtime["auth"]["email"], runtime["auth"]["project_id"] = "owner@example.test", "project-fixture"
	runtime["auth"]["access_token"], runtime["auth"]["updated_at"], runtime["auth"]["mtime"] = "rotated-oauth-token", "second-update", 9876
	raw, _ := json.Marshal(runtime)
	host.runtime = string(raw)
	refreshed, err := cpa.ResolveGenerationAccount(context.Background(), "a")
	if err != nil || refreshed.Binding != account.Binding {
		t.Fatalf("OAuth refresh changed binding=%+v err=%v", refreshed, err)
	}
	raw, _ = json.Marshal(refreshed)
	if strings.Contains(string(raw), "owner@") || strings.Contains(string(raw), "project-fixture") || strings.Contains(string(raw), "oauth") || strings.Contains(string(raw), account.Binding) || strings.Contains(string(raw), "auth_index") {
		t.Fatalf("resolved account leaked metadata: %s", raw)
	}
	if account.Binding == bindingID(account, "owner@example.test") {
		t.Fatal("provider namespaces overlap")
	}
	runtime["auth"]["project_id"] = ""
	raw, _ = json.Marshal(runtime)
	host.runtime = string(raw)
	projectless, err := cpa.ResolveGenerationAccount(context.Background(), "a")
	if err != nil || projectless.Binding == account.Binding || projectless.Provider != "antigravity" || host.credentials != 0 {
		t.Fatalf("project optional account=%+v err=%v", projectless, err)
	}
}

func TestAntigravityRuntimeValidation(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
		code  string
	}{
		{"id replacement", "id", "replacement", "account_binding_changed"},
		{"index replacement", "auth_index", "replacement", "account_binding_changed"},
		{"provider replacement", "provider", "codex", "account_binding_changed"},
		{"runtime only", "runtime_only", true, "account_binding_changed"},
		{"disabled", "disabled", true, "account_disabled"},
		{"disabled status", "status", "disabled", "account_disabled"},
		{"unavailable", "unavailable", true, "account_identity_unavailable"},
		{"unavailable status", "status", "unavailable", "account_identity_unavailable"},
		{"missing email", "email", nil, "account_identity_unavailable"},
		{"blank email", "email", "  ", "account_identity_unavailable"},
		{"long email", "email", strings.Repeat("a", 4097), "account_identity_unavailable"},
		{"email control", "email", "user\x00@example.test", "account_identity_unavailable"},
		{"long project", "project_id", strings.Repeat("a", 4097), "account_identity_unavailable"},
		{"project control", "project_id", "project\nsecret", "account_identity_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, _ := antigravityFixture(t)
			var runtime map[string]map[string]any
			_ = json.Unmarshal([]byte(host.runtime), &runtime)
			runtime["auth"][test.field] = test.value
			raw, _ := json.Marshal(runtime)
			host.runtime = string(raw)
			account, err := cpa.ResolveGenerationAccount(context.Background(), "a")
			if err == nil || err.Error() != test.code || account.ID != "" || host.credentials != 0 || host.executes != 0 {
				t.Fatalf("account=%+v err=%v credentials=%d executes=%d", account, err, host.credentials, host.executes)
			}
		})
	}
}

func TestAntigravityGenerationPinsProviderAndPreservesStreaming(t *testing.T) {
	completed := string(generationCompleted(`["First.","Second."]`))
	for _, framing := range []string{"json", "SSE"} {
		t.Run(framing, func(t *testing.T) {
			cpa, host, account := antigravityFixture(t)
			if framing == "SSE" {
				host.chunks = []generationRead{{Payload: []byte("event: response.completed\ndata: " + completed[:36])}, {Payload: []byte(completed[36:] + "\n\n"), Done: true}}
			}
			values, err := cpa.GenerateCandidates(context.Background(), account, "gemini-fixture", 2)
			if err != nil || !reflect.DeepEqual(values, []string{"First.", "Second."}) || host.executes != 1 || host.closes != 1 || host.credentials != 0 || host.runtimes != 2 {
				t.Fatalf("values=%v err=%v model=%d close=%d runtime=%d credential=%d", values, err, host.executes, host.closes, host.runtimes, host.credentials)
			}
			request := host.request
			if request.ForcedProvider != "antigravity" || request.EntryProtocol != "openai-response" || request.ExitProtocol != "openai-response" || request.AuthID != "a" || request.Model != "gemini-fixture" || !request.Stream {
				t.Fatalf("unsafe provider request=%+v", request)
			}
			var body map[string]any
			if json.Unmarshal(host.requestBody, &body) != nil || body["store"] != false || body["stream"] != true || body["model"] != "gemini-fixture" || strings.Contains(string(host.requestBody), "owner@") || strings.Contains(string(host.requestBody), "secret-token") {
				t.Fatalf("unsafe body=%s", host.requestBody)
			}
		})
	}
}

func TestAntigravityGenerationRevalidatesSnapshotAndNeverFallsBack(t *testing.T) {
	for _, field := range []string{"email", "project_id", "auth_index", "provider"} {
		t.Run(field, func(t *testing.T) {
			cpa, host, account := antigravityFixture(t)
			var runtime map[string]map[string]any
			_ = json.Unmarshal([]byte(host.runtime), &runtime)
			runtime["auth"][field] = "replacement"
			raw, _ := json.Marshal(runtime)
			host.runtime = string(raw)
			if _, err := cpa.GenerateCandidates(context.Background(), account, "gemini-fixture", 2); err == nil || err.Error() != "account_binding_changed" || host.executes != 0 || host.closes != 0 {
				t.Fatalf("replacement err=%v executes=%d close=%d", err, host.executes, host.closes)
			}
		})
	}
	cpa, host, account := antigravityFixture(t)
	cpa.Host = host
	beforeLists, beforeRuntimes := host.lists, host.runtimes
	if _, err := cpa.GenerateCandidates(context.Background(), account, "gemini-fixture", 2); err == nil || err.Error() != "generation_not_supported" || host.executes != 0 || host.lists != beforeLists || host.runtimes != beforeRuntimes {
		t.Fatalf("legacy bridge err=%v executes=%d", err, host.executes)
	}
	cpa, host, account = antigravityFixture(t)
	account.Provider = "codex"
	if _, err := cpa.GenerateCandidates(context.Background(), account, "gemini-fixture", 2); err == nil || err.Error() != "account_binding_changed" || host.executes != 0 {
		t.Fatalf("cross-provider snapshot err=%v executes=%d", err, host.executes)
	}
}

func TestAntigravityGenerationUnknownNeverRetriesAndAlwaysCloses(t *testing.T) {
	cpa, host, account := antigravityFixture(t)
	host.executeErr = errors.New("private upstream failure")
	if values, err := cpa.GenerateCandidates(context.Background(), account, "gemini-fixture", 2); values != nil || err == nil || err.Error() != "generation_delivery_unknown" || host.executes != 1 || host.closes != 1 {
		t.Fatalf("values=%v err=%v execute=%d close=%d", values, err, host.executes, host.closes)
	}
}

func TestCodexGenerationResolverRemainsLegacyBindingCompatible(t *testing.T) {
	cpa, host, snapshot := generationFixture()
	account, err := cpa.ResolveGenerationAccount(context.Background(), snapshot.ID)
	if err != nil || account.Provider != "codex" || account.Binding != snapshot.Binding || account.AuthIndex != snapshot.AuthIndex || host.credentials != 1 {
		t.Fatalf("account=%+v err=%v credentials=%d", account, err, host.credentials)
	}
	for _, status := range []string{"disabled", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			cpa, host, _ := generationFixture()
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","status":"` + status + `"}}`
			_, err := cpa.ResolveGenerationAccount(context.Background(), "a")
			if err == nil || host.credentials != 0 || host.executes != 0 {
				t.Fatalf("runtime status accepted err=%v credentials=%d", err, host.credentials)
			}
		})
	}
}
