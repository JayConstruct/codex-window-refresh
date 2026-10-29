package executor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type catalogHost struct {
	instance string
	provider string
	result   string
	err      error
	change   string
	calls    []string
	runtimes int
}

func (h *catalogHost) Instance() string { return h.instance }
func (h *catalogHost) Call(_ context.Context, method string, request, response any) error {
	h.calls = append(h.calls, method)
	provider := h.provider
	if provider == "" {
		provider = "codex"
	}
	raw := ""
	switch method {
	case "host.auth.list":
		raw = `{"files":[{"id":"a","auth_index":"i","provider":"` + provider + `"}]}`
	case "host.auth.get_runtime":
		h.runtimes++
		id := "a"
		if h.runtimes > 1 && h.change == "account" {
			id = "replacement"
		}
		raw = `{"auth":{"id":"` + id + `","auth_index":"i","provider":"` + provider + `"}}`
	case "host.auth.models":
		encoded, _ := json.Marshal(request)
		if string(encoded) != `{"auth_index":"i"}` {
			return errors.New("unexpected unpinned catalog request")
		}
		if h.err != nil {
			return h.err
		}
		if h.change == "instance" {
			h.instance = "replacement"
		}
		raw = h.result
		if raw == "" {
			raw = `{"credentials":[{"id":"a","auth_index":"i","provider":"` + provider + `","models":["first","second","first"]}]}`
		}
	default:
		return errors.New("unexpected credential or network callback")
	}
	return json.Unmarshal([]byte(raw), response)
}

func TestAccountModelsPinsProviderAndNeverReadsCredentials(t *testing.T) {
	for _, provider := range []string{"codex", "antigravity"} {
		host := &catalogHost{instance: "one", provider: provider}
		cpa := &CPA{Host: host}
		catalog, err := cpa.AccountModels(context.Background(), "a")
		if err != nil || catalog.AccountID != "a" || catalog.Provider != provider || !reflect.DeepEqual(catalog.Models, []string{"first", "second"}) {
			t.Fatal(catalog, err)
		}
		if !reflect.DeepEqual(host.calls, []string{"host.auth.list", "host.auth.get_runtime", "host.auth.models", "host.auth.get_runtime"}) {
			t.Fatal(host.calls)
		}
		raw, _ := json.Marshal(catalog)
		if strings.Contains(string(raw), "auth_index") {
			t.Fatal("private host index escaped", string(raw))
		}
	}
}

func TestAccountModelsRejectsWrongBindingsMalformedAndOversizedModels(t *testing.T) {
	for _, result := range []string{
		`{"credentials":[]}`,
		`{"credentials":[{"id":"other","auth_index":"i","provider":"codex","models":[]}]}`,
		`{"credentials":[{"id":"a","auth_index":"other","provider":"codex","models":[]}]}`,
		`{"credentials":[{"id":"a","auth_index":"i","provider":"antigravity","models":[]}]}`,
		`{"credentials":[{"id":"a","auth_index":"i","provider":"codex","models":null}]}`,
		`{"credentials":[{"id":"a","auth_index":"i","provider":"codex","models":["bad model"]}]}`,
		`{"credentials":[{"id":"a","auth_index":"i","provider":"codex","models":["` + strings.Repeat("x", 201) + `"]}]}`,
		`{"credentials":[{"id":"a","auth_index":"i","provider":"codex","models":[` + strings.Repeat(`"m",`, 2048) + `"m"]}]}`,
		`{"ignored":"` + strings.Repeat("x", 512<<10) + `","credentials":[]}`,
	} {
		host := &catalogHost{instance: "one", result: result}
		catalog, err := (&CPA{Host: host}).AccountModels(context.Background(), "a")
		if err == nil || catalog.AccountID != "" || len(catalog.Models) != 0 {
			t.Fatal("unsafe catalog accepted", err)
		}
	}
}

func TestAccountModelsRejectsConcurrentReplacementAndDoesNotFallbackOnFailures(t *testing.T) {
	for _, change := range []string{"account", "instance"} {
		host := &catalogHost{instance: "one", change: change}
		if _, err := (&CPA{Host: host}).AccountModels(context.Background(), "a"); err == nil || err.Error() != "model_catalog_changed" {
			t.Fatal(change, err)
		}
	}
	for _, test := range []struct{ message, expected string }{
		{"model_catalog_unsupported", "model_catalog_unsupported"},
		{"method_not_allowed", "model_catalog_unsupported"},
		{"private credential error", "model_catalog_unavailable"},
	} {
		host := &catalogHost{instance: "one", err: errors.New(test.message)}
		if _, err := (&CPA{Host: host}).AccountModels(context.Background(), "a"); err == nil || err.Error() != test.expected {
			t.Fatal(test, err)
		}
	}
}
