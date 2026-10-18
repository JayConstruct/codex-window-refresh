package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

func TestQuotaQueryReturnsBoundRuntimeStatusWithoutModelRequests(t *testing.T) {
	for _, scenario := range []string{"disabled during query", "recovered during query", "authentication failed", "transport failed", "legacy omitted", "binding changed"} {
		t.Run(scenario, func(t *testing.T) {
			host := &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: "active"}}
			account := store.Account{ID: "a", AuthIndex: "i", Status: "available"}
			account.Binding = bindingID(account, "upstream")
			wanted := "disabled"
			code := ""
			if scenario == "recovered during query" {
				host.entry.Status = "error"
				wanted = "available"
			}
			if scenario == "legacy omitted" {
				host.entry.Status = ""
				wanted = ""
			}
			if scenario == "transport failed" {
				host.entry.Status = "disabled"
			}
			cpa := &CPA{Host: host, transport: roundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
					t.Fatal("query performed a model request")
				}
				switch scenario {
				case "disabled during query":
					host.entry.Status = "disabled"
				case "recovered during query":
					host.entry.Status = "active"
				case "authentication failed":
					host.entry.Status = "disabled"
					code = "quota_authentication_failed"
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private upstream failure"))}, nil
				case "transport failed":
					code = "quota_transport_failed"
					return nil, errors.New("private proxy credentials")
				case "binding changed":
					host.entry.Index = "replacement"
					code = "account_binding_changed"
					wanted = ""
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
			})}
			raw, observed, err := cpa.QueryWithStatus(context.Background(), account)
			if code != "" && (err == nil || err.Error() != code) || code == "" && err != nil {
				t.Fatalf("raw=%s observed=%+v err=%v", raw, observed, err)
			}
			if wanted == "" {
				if observed != nil {
					t.Fatal("accepted unbound or omitted status", observed)
				}
			} else {
				if observed == nil || observed.Status != wanted || observed.ID != account.ID || observed.AuthIndex != account.AuthIndex || observed.Binding != account.Binding {
					t.Fatal("missing bound runtime status", observed)
				}
				at, parseErr := time.Parse(time.RFC3339Nano, observed.StatusObservedAt)
				if parseErr != nil || at.IsZero() || at.After(time.Now().UTC()) {
					t.Fatal(observed.StatusObservedAt, parseErr)
				}
			}
			for _, method := range host.methods {
				if method != "host.auth.get_runtime" && method != "host.auth.get" {
					t.Fatal("unexpected host side effect", method)
				}
			}
		})
	}
}

type missingQuotaTokenHost struct {
	*statusHost
}

func (host *missingQuotaTokenHost) Call(ctx context.Context, method string, request, response any) error {
	if method == "host.auth.get" {
		host.methods = append(host.methods, method)
		return json.Unmarshal([]byte(`{"auth_index":"i","json":{"type":"codex","account_id":"upstream"}}`), response)
	}
	return host.statusHost.Call(ctx, method, request, response)
}

func TestQuotaPreservesVerifiedHostStatusWithoutAToken(t *testing.T) {
	host := &missingQuotaTokenHost{statusHost: &statusHost{entry: authEntry{ID: "a", Index: "i", Provider: "codex", Status: "disabled"}}}
	account := store.Account{ID: "a", AuthIndex: "i", Status: "available"}
	account.Binding = bindingID(account, "upstream")
	cpa := &CPA{Host: host, transport: roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing token sent an upstream request")
		return nil, nil
	})}
	_, observed, err := cpa.QueryWithStatus(context.Background(), account)
	if err == nil || err.Error() != "quota_credential_missing" || observed == nil || observed.Status != "disabled" || observed.Binding != account.Binding {
		t.Fatal("missing token lost the independently verified host state", observed, err)
	}
}
