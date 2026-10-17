package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"codex-window-refresh/internal/v2/store"
)

type statusHost struct {
	entry   authEntry
	methods []string
}

func (host *statusHost) Call(_ context.Context, method string, _ any, response any) error {
	host.methods = append(host.methods, method)
	var value any
	switch method {
	case "host.auth.list":
		value = map[string]any{"files": []authEntry{host.entry}}
	case "host.auth.get_runtime":
		value = map[string]any{"auth": host.entry}
	case "host.auth.get":
		value = map[string]any{"auth_index": host.entry.Index, "json": map[string]string{"type": "codex", "account_id": "upstream", "access_token": "fixture-token"}}
	default:
		panic("unexpected side effect " + method)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, response)
}

func TestAccountsMapsHostLifecycleStatusesWithoutUpstreamRequests(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      string
		disabled    bool
		unavailable bool
		wanted      string
	}{
		{name: "active", status: "active", wanted: "available"},
		{name: "omitted legacy status", wanted: "available"},
		{name: "disabled status", status: "disabled", wanted: "disabled"},
		{name: "disabled flag", status: "active", disabled: true, wanted: "disabled"},
		{name: "disabled over unavailable", status: "disabled", unavailable: true, wanted: "disabled"},
		{name: "disabled flag over error", status: "error", disabled: true, unavailable: true, wanted: "disabled"},
		{name: "unavailable flag", status: "active", unavailable: true, wanted: "unavailable"},
		{name: "error", status: "error", wanted: "unavailable"},
		{name: "pending", status: "pending", wanted: "unavailable"},
		{name: "refreshing", status: "refreshing", wanted: "unavailable"},
		{name: "unknown", status: "unknown", wanted: "unavailable"},
		{name: "unsupported", status: "future_state", wanted: "unavailable"},
		{name: "legacy unavailable", status: "unavailable", wanted: "unavailable"},
		{name: "noncanonical active", status: " active ", wanted: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: test.status, Disabled: test.disabled, Unavailable: test.unavailable}}
			cpa := &CPA{Host: host, transport: roundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("inventory issued an upstream request")
				return nil, nil
			})}
			accounts, err := cpa.Accounts(context.Background())
			if err != nil || len(accounts) != 1 || accounts[0].Status != test.wanted || accounts[0].AuthIndex != "i" {
				t.Fatalf("accounts=%+v err=%v", accounts, err)
			}
			if !reflect.DeepEqual(host.methods, []string{"host.auth.list"}) {
				t.Fatalf("inventory side effects=%v", host.methods)
			}
		})
	}
}

func TestAccountsBindingSafetyOverridesLifecycleStatus(t *testing.T) {
	for _, entry := range []authEntry{
		{ID: "a", Provider: "codex", Status: "disabled"},
		{ID: "a", Index: "i", Provider: "codex", Status: "error", RuntimeOnly: true},
		{ID: "a", Index: "i", Provider: "codex", Status: "active", Disabled: true, RuntimeOnly: true},
	} {
		host := &statusHost{entry: entry}
		accounts, err := (&CPA{Host: host}).Accounts(context.Background())
		if err != nil || len(accounts) != 1 || accounts[0].Status != "binding_unavailable" || accounts[0].AuthIndex != "" {
			t.Fatalf("entry=%+v accounts=%+v err=%v", entry, accounts, err)
		}
	}
}

func TestIdentityUsesLatestExplicitRuntimeStatusWithoutChangingBinding(t *testing.T) {
	for _, test := range []struct {
		name        string
		previous    string
		status      string
		disabled    bool
		unavailable bool
		wanted      string
	}{
		{name: "disabled after list", previous: "available", status: "disabled", wanted: "disabled"},
		{name: "error after list", previous: "available", status: "error", wanted: "unavailable"},
		{name: "pending after list", previous: "available", status: "pending", wanted: "unavailable"},
		{name: "refreshing after list", previous: "available", status: "refreshing", wanted: "unavailable"},
		{name: "unknown after list", previous: "available", status: "unknown", wanted: "unavailable"},
		{name: "re-enabled after list", previous: "disabled", status: "active", wanted: "available"},
		{name: "recovered after list", previous: "unavailable", status: "active", wanted: "available"},
		{name: "disabled boolean", previous: "available", disabled: true, wanted: "disabled"},
		{name: "unavailable boolean", previous: "available", unavailable: true, wanted: "unavailable"},
		{name: "legacy omitted runtime status", previous: "disabled", wanted: "disabled"},
		{name: "binding safety", previous: "binding_unavailable", status: "active", wanted: "binding_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: test.status, Disabled: test.disabled, Unavailable: test.unavailable}}
			account := store.Account{ID: "a", AuthIndex: "i", Status: test.previous}
			account.Binding = bindingID(account, "upstream")
			identity, err := (&CPA{Host: host}).Identity(context.Background(), account)
			if err != nil || identity.Status != test.wanted || identity.Binding != account.Binding || identity.ID != account.ID || identity.AuthIndex != account.AuthIndex {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
			if !reflect.DeepEqual(host.methods, []string{"host.auth.get_runtime", "host.auth.get"}) {
				t.Fatalf("identity side effects=%v", host.methods)
			}
		})
	}
}

func TestRequireAvailableRejectsNonactiveRuntimeBeforeReadingCredentials(t *testing.T) {
	for _, status := range []string{"disabled", "error", "pending", "refreshing", "unknown", "future_state"} {
		t.Run(status, func(t *testing.T) {
			host := &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: status}}
			_, err := (&CPA{Host: host}).readBindingFor(context.Background(), store.Account{ID: "a", AuthIndex: "i"}, true)
			wanted := "account_identity_unavailable"
			if status == "disabled" {
				wanted = "account_disabled"
			}
			if err == nil || err.Error() != wanted || !reflect.DeepEqual(host.methods, []string{"host.auth.get_runtime"}) {
				t.Fatalf("err=%v methods=%v", err, host.methods)
			}
		})
	}
}

func TestReadOnlyQuotaAllowsHostLifecycleErrors(t *testing.T) {
	for _, status := range []string{"disabled", "error", "pending", "refreshing", "unknown"} {
		t.Run(status, func(t *testing.T) {
			host := &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: status}}
			queries := 0
			cpa := &CPA{Host: host, transport: roundTrip(func(request *http.Request) (*http.Response, error) {
				queries++
				if request.Method != http.MethodGet || request.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
					t.Fatal("unexpected quota request")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
			})}
			if _, err := cpa.Query(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: accountStatus(host.entry)}); err != nil || queries != 1 {
				t.Fatalf("err=%v queries=%d", err, queries)
			}
			if !reflect.DeepEqual(host.methods, []string{"host.auth.get_runtime", "host.auth.get", "host.auth.get_runtime", "host.auth.get"}) {
				t.Fatalf("quota callback side effects=%v", host.methods)
			}
		})
	}
}
