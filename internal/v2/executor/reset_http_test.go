package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

func resetHTTPAccount() store.Account {
	a := store.Account{ID: "a", AuthIndex: "i", Status: "available"}
	a.Binding = bindingID(a, "upstream")
	return a
}

func TestResetHTTPUsesOwnedCPARequestsWithoutCodexAndPinsCardAndIdempotency(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	host := &resetRuntimeHost{runtime: `{"auth":{"id":"a","auth_index":"i","provider":"codex","unavailable":true}}`}
	opened, cancelled := 0, 0
	requests := []quotaHTTPRequest{}
	callback := quotaCallback(func(ctx context.Context, method string, request, response any) error {
		switch method {
		case "host.http.operation_open":
			opened++
			return quotaRespond(response, map[string]string{"operation_id": "reset-http"})
		case "host.http.cancel":
			cancelled++
			return quotaRespond(response, map[string]any{})
		case "host.http.do":
			r := request.(quotaHTTPRequest)
			requests = append(requests, r)
			if r.Headers.Get("Authorization") != "Bearer fixture-token" || r.Headers.Get("Chatgpt-Account-Id") != "upstream" || r.Headers.Get("OpenAI-Beta") != "codex-1" || r.Headers.Get("Originator") != "Codex Desktop" {
				t.Fatal("unbound request", r.Headers)
			}
			body := `{"code":"already_redeemed"}`
			if r.Method == "GET" {
				if r.URL != resetCreditsURL || len(r.Body) != 0 {
					t.Fatal(r)
				}
				body = `{"available_count":1,"credits":[{"id":"card-1","reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-01T00:00:00Z","title":"private-free-text","access_token":"private-token"}]}`
			} else {
				if r.Method != "POST" || r.URL != resetConsumeURL {
					t.Fatal(r)
				}
				var sent map[string]string
				if json.Unmarshal(r.Body, &sent) != nil || !reflect.DeepEqual(sent, map[string]string{"credit_id": "card-1", "redeem_request_id": "persisted-attempt"}) {
					t.Fatal(string(r.Body))
				}
			}
			return quotaRespond(response, quotaHTTPResponse{StatusCode: 200, Body: []byte(body)})
		default:
			return host.Call(ctx, method, request, response)
		}
	})
	c := &CPA{Host: callback}
	if transport, err := c.ResetTransport(); err != nil || transport != "cpa_http" {
		t.Fatal(transport, err)
	}
	a := resetHTTPAccount()
	cards, err := c.ReadResetCredits(context.Background(), a)
	if err != nil || cards == nil || cards.AvailableCount != 1 || len(cards.Credits) != 1 || cards.Credits[0].ResetType != "codexRateLimits" || cards.Credits[0].ExpiresAt == nil {
		t.Fatal(cards, err)
	}
	raw, _ := json.Marshal(cards)
	if strings.Contains(string(raw), "private") {
		t.Fatal("private upstream fields retained", string(raw))
	}
	for range 2 {
		outcome, err := c.ConsumeResetCredit(context.Background(), a, "card-1", "persisted-attempt")
		if err != nil || outcome != "alreadyRedeemed" {
			t.Fatal(outcome, err)
		}
	}
	if opened != 3 || cancelled != 3 || len(requests) != 3 || len(c.transports) != 0 {
		t.Fatal(opened, cancelled, len(requests))
	}
}

func TestResetHTTPResponseCodesAndAmbiguousResponses(t *testing.T) {
	for _, tc := range []struct {
		body          string
		status        int
		outcome, code string
	}{
		{`{"code":"reset"}`, 200, "reset", ""},
		{`{"code":"already_redeemed"}`, 200, "alreadyRedeemed", ""},
		{`{"code":"no_credit"}`, 200, "noCredit", ""},
		{`{"code":"nothing_to_reset"}`, 200, "nothingToReset", ""},
		{"", 200, "", "reset_protocol_invalid"},
		{`{"code":"unexpected"}`, 200, "", "reset_protocol_invalid"},
		{"private-error-body", 401, "", "reset_authentication_failed"},
		{"private-error-body", 403, "", "reset_permission_denied"},
		{"private-error-body", 429, "", "reset_rate_limited"},
		{"private-error-body", 500, "", "reset_upstream_failed"},
	} {
		t.Run(tc.outcome+tc.code+tc.body[:min(len(tc.body), 10)], func(t *testing.T) {
			c := &CPA{Host: &fixtureHost{}, transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			outcome, err := c.ConsumeResetCredit(context.Background(), resetHTTPAccount(), "card", "same-attempt")
			if outcome != tc.outcome || tc.code == "" && err != nil || tc.code != "" && (err == nil || err.Error() != tc.code) {
				t.Fatal(outcome, err)
			}
		})
	}
}

func TestResetHTTPBindingAndDisableFencesBeforeAndAfterSend(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		host := &resetRuntimeHost{fixtureHost: fixtureHost{changed: !disabled}, runtime: `{"auth":{"id":"a","auth_index":"i","provider":"codex","disabled":` + map[bool]string{true: "true", false: "false"}[disabled] + `}}`}
		calls := 0
		c := &CPA{Host: host, transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"reset"}`))}, nil
		})}
		_, err := c.ConsumeResetCredit(context.Background(), resetHTTPAccount(), "card", "same-key")
		if disabled {
			if err == nil || err.Error() != "account_disabled" || calls != 0 {
				t.Fatal(err, calls)
			}
		} else if err == nil || err.Error() != "account_binding_changed" || calls != 1 {
			t.Fatal(err, calls)
		}
	}
}

func TestResetHTTPNeverRetriesMutationOrFallsBackAfterLostResponse(t *testing.T) {
	calls := 0
	c := &CPA{Host: &fixtureHost{}, transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("private transport details")
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.ConsumeResetCredit(ctx, resetHTTPAccount(), "card", "persisted-key"); err == nil || err.Error() != "reset_transport_failed" || calls != 1 {
		t.Fatal(err, calls)
	}
}
