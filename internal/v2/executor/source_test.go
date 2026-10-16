package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-window-refresh/internal/v2/store"
)

type fixtureHost struct {
	changed   bool
	gets      int
	inventory string
}

func (f *fixtureHost) Call(_ context.Context, method string, req, response any) error {
	raw := `{}`
	switch method {
	case "host.auth.list":
		raw = `{"files":[{"id":"a","auth_index":"i","provider":"codex"}]}`
		if f.inventory != "" {
			raw = f.inventory
		}
	case "host.auth.get_runtime":
		raw = `{"auth":{"id":"a","auth_index":"i","provider":"codex"}}`
	case "host.auth.get":
		f.gets++
		upstream := "upstream"
		if f.changed && f.gets > 1 {
			upstream = "replacement"
		}
		raw = `{"auth_index":"i","json":{"type":"codex","account_id":"` + upstream + `","access_token":"fixture-token"}}`
	default:
		tpanic(method)
	}
	return json.Unmarshal([]byte(raw), response)
}

func TestMalformedAccountDoesNotBlockHealthyInventory(t *testing.T) {
	c := &CPA{Host: &fixtureHost{inventory: `{"files":[{"id":"good","auth_index":"g","provider":"codex"},{"id":"bad","auth_index":"b1","provider":"codex"},{"id":"bad","auth_index":"b2","provider":"codex"},{"id":"runtime","auth_index":"r","provider":"codex","runtime_only":true}]}`}}
	accounts, err := c.Accounts(context.Background())
	if err != nil || len(accounts) != 3 {
		t.Fatalf("inventory=%+v err=%v", accounts, err)
	}
	for _, a := range accounts {
		if a.ID == "good" {
			if a.Status != "available" || a.AuthIndex != "g" {
				t.Fatal("healthy account blocked")
			}
		} else if a.Status != "binding_unavailable" || a.AuthIndex != "" {
			t.Fatal("invalid account received executable identity")
		}
	}
}
func tpanic(method string) { panic("unexpected side effect " + method) }

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestQueryNormalizesAndDoesNotPersistCredentials(t *testing.T) {
	host := &fixtureHost{}
	c := &CPA{Host: host, transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Chatgpt-Account-Id") != "upstream" {
			t.Fatal("unbound quota request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
	})}
	quota, err := c.Query(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: "available"})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(quota) || strings.Contains(string(quota), "fixture-token") || strings.Contains(string(quota), "access_token") {
		t.Fatal("invalid or secret-bearing quota snapshot")
	}
	host.changed = true
	host.gets = 0
	if _, err = c.Query(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: "available"}); err == nil || err.Error() != "account_binding_changed" {
		t.Fatal("account replacement during query accepted")
	}
}

func TestKnownBindingRejectsReplacementBeforeHTTP(t *testing.T) {
	host := &fixtureHost{}
	c := &CPA{Host: host, transport: roundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("query escaped binding fence"); return nil, nil })}
	a, err := c.Identity(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: "available"})
	if err != nil || a.Binding == "" {
		t.Fatal(err)
	}
	host.changed = true
	if _, err = c.Query(context.Background(), a); err == nil || err.Error() != "account_binding_changed" {
		t.Fatal("replacement was not rejected", err)
	}
}
