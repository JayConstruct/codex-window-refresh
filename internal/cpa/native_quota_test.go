package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func nativeTestBinding() AuthBinding {
	return AuthBinding{AccountID: "host-auth", AuthIndex: "auth-index", UpstreamAccountID: "real", Generation: generation("host-auth", "real")}
}

// RPC names below deliberately use independent literals from the pinned CPA
// sdk/pluginabi/types.go. Reusing implementation constants would hide a typo in
// the host contract, such as operation.open instead of operation_open.
func TestNativeQuotaReusesHostCredentialsAndReadsRotatedTokenWithoutManagementKey(t *testing.T) {
	f := &fakeCallbacks{upstream: "real", token: "private-first-token"}
	reads := 0
	cb := callbackFunc(func(ctx context.Context, method string, req, out any) error {
		switch method {
		case "host.http.operation_open":
			return respond(out, map[string]string{"operation_id": "query-op"})
		case "host.http.do":
			request := req.(quotaHTTPRequest)
			if request.Headers.Get("User-Agent") != codexQuotaUserAgent || request.Headers.Get("Content-Type") != "application/json" {
				t.Fatal("host quota request lost its request identity")
			}
			if request.Method != "GET" || request.URL != codexUsageURL || request.OperationID != "query-op" || request.Headers.Get("Authorization") != "Bearer "+f.token || request.Headers.Get("Chatgpt-Account-Id") != "real" {
				t.Fatal("quota query lost its account binding or sent the wrong request")
			}
			reads++
			body, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{"allowed": true, "primary_window": map[string]any{"limit_window_seconds": 18000, "used_percent": 20, "reset_at": time.Now().Add(time.Hour).Unix()}}})
			return respond(out, quotaHTTPResponse{200, body})
		default:
			if strings.HasPrefix(method, "host.model.") || method == "host.auth.save" {
				t.Fatal("quota reader dispatched a model or rotated OAuth credentials")
			}
			return f.Call(ctx, method, req, out)
		}
	})
	t.Setenv("CPA_MANAGEMENT_KEY", "")
	h := NewHost(cb, &NativeReader{Callback: cb})
	pool, err := h.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"private-first-token", "private-rotated-token"} {
		f.token = token
		observation, err := h.ObserveQuota(context.Background(), core.QuotaRead{AccountID: pool[0].ID, Generation: pool[0].Generation})
		if err != nil || observation.FiveHour == nil || observation.FiveHour.Remaining.Value != 80 || observation.Source != "cpa_wham_usage" {
			t.Fatal("native quota did not parse a bound observation")
		}
	}
	if reads != 2 {
		t.Fatal("fresh credentials were not queried exactly once")
	}
	f.upstream = "replaced-account"
	if _, err := h.ObserveQuota(context.Background(), core.QuotaRead{AccountID: pool[0].ID, Generation: pool[0].Generation}); !errors.Is(err, ErrBindingChanged) || reads != 2 {
		t.Fatal("replaced account used stale token binding")
	}
}

func TestNativeQuotaRejectsCredentialsBeforeNetworkAndSanitizesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		status           int
		expected         error
	}{
		{"missing token", `{"type":"codex","account_id":"real"}`, 200, ErrQuotaCredentialMissing},
		{"expired", `{"type":"codex","account_id":"real","access_token":"private","expired":"2000-01-01T00:00:00Z"}`, 200, ErrQuotaCredentialExpired},
		{"wrong account", `{"type":"codex","account_id":"other","access_token":"private"}`, 200, ErrBindingChanged},
		{"wrong provider", `{"type":"claude","account_id":"real","access_token":"private"}`, 200, ErrBindingChanged},
		{"invalid expiry", `{"type":"codex","account_id":"real","access_token":"private","expired":"not-a-date"}`, 200, ErrQuotaCredentialMissing},
		{"header injection", `{"type":"codex","account_id":"real","access_token":"private\r\nX-Test: injected"}`, 200, ErrQuotaCredentialMissing},
		{"upstream 401", `{"type":"codex","account_id":"real","access_token":"private"}`, 401, ErrQuotaCredentialExpired},
		{"upstream 403", `{"type":"codex","account_id":"real","access_token":"private"}`, 403, ErrQuotaForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			cb := callbackFunc(func(_ context.Context, method string, req, out any) error {
				switch method {
				case "host.auth.get":
					return respond(out, authGet{AuthIndex: "auth-index", JSON: json.RawMessage(tc.credential)})
				case "host.http.operation_open":
					return respond(out, map[string]string{"operation_id": "test"})
				case "host.http.do":
					reads++
					return respond(out, quotaHTTPResponse{tc.status, []byte(`{"private_error":"must-not-leak"}`)})
				default:
					return errors.New("unexpected callback with private detail")
				}
			})
			_, err := (&NativeReader{Callback: cb}).Read(context.Background(), nativeTestBinding())
			if !errors.Is(err, tc.expected) || strings.Contains(err.Error(), "private") || (tc.status == 200 && reads != 0) || (tc.status != 200 && reads != 1) {
				t.Fatal("credential/error handling leaked data or sent an unexpected request")
			}
		})
	}
}

func TestNativeQuotaUsesAccountProxyWithoutHostHTTPAndBoundsResponses(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		cb := callbackFunc(func(_ context.Context, method string, req, out any) error {
			if method != "host.auth.get" {
				t.Fatal("account proxy unexpectedly used host HTTP or model callbacks")
			}
			return respond(out, authGet{AuthIndex: "auth-index", JSON: json.RawMessage(`{"type":"codex","account_id":"real","access_token":"private","proxy_url":"socks5://user:secret@proxy.invalid:1080"}`)})
		})
		reads := 0
		reader := &NativeReader{Callback: cb, TransportForProxy: func(proxy string) (http.RoundTripper, error) {
			if proxy != "socks5://user:secret@proxy.invalid:1080" {
				t.Fatal("account proxy was not preserved")
			}
			return roundTripFunc(func(req *http.Request) (*http.Response, error) {
				reads++
				if req.URL.String() != codexUsageURL || req.Header.Get("Authorization") != "Bearer private" {
					t.Fatal("wrong proxy quota request")
				}
				body := `{"rate_limit":{"allowed":true}}`
				if oversized {
					body = strings.Repeat("x", maxQuotaBody+1)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		}}
		body, err := reader.Read(context.Background(), nativeTestBinding())
		if reads != 1 || (oversized && !errors.Is(err, ErrQuotaResponseTooLarge)) || (!oversized && (err != nil || !json.Valid(body))) {
			t.Fatal("proxy query or response limit failed")
		}
	}
	for _, proxy := range []string{"direct", "none", "http://localhost:8888", "https://localhost:443", "socks5://localhost:1080", "socks5h://localhost:1080"} {
		transport, err := nativeProxyTransport(proxy)
		if err != nil {
			t.Fatal("supported proxy rejected")
		}
		if (proxy == "direct" || proxy == "none") && transport.(*http.Transport).Proxy != nil {
			t.Fatal("explicit direct inherited an environment proxy")
		}
	}
	for _, proxy := range []string{"file:///secret", "http://localhost:99999", "http://private:secret@localhost/?token=private"} {
		if _, err := nativeProxyTransport(proxy); !errors.Is(err, ErrQuotaProxyInvalid) {
			t.Fatal("invalid proxy accepted or leaked its URL")
		}
	}
}

func TestNativeHostHTTPDeadlineCancelsHostOperation(t *testing.T) {
	entered, cancelled, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	cb := callbackFunc(func(_ context.Context, method string, req, out any) error {
		switch method {
		case "host.http.operation_open":
			return respond(out, map[string]string{"operation_id": "cancel-op"})
		case "host.http.do":
			close(entered)
			<-cancelled
			return errors.New("secret upstream error")
		case "host.http.cancel":
			if req.(map[string]string)["operation_id"] != "cancel-op" {
				t.Fatal("cancelled wrong operation")
			}
			once.Do(func() { close(cancelled) })
			return nil
		default:
			return errors.New("unexpected callback")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := (&NativeReader{Callback: cb}).hostHTTP(ctx, http.Header{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("host request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQuotaCancelled) {
			t.Fatal("cancellation leaked an upstream error")
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not cancel the host operation")
	}
}
