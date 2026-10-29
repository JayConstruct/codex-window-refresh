package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type quotaCallback func(context.Context, string, any, any) error

func (callback quotaCallback) Call(ctx context.Context, method string, request, response any) error {
	return callback(ctx, method, request, response)
}

func quotaRespond(response, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, response)
}

func TestQuotaInheritsHostProxyPolicyWithFreshCredentials(t *testing.T) {
	fixture := &fixtureHost{}
	opened, sent, cancelled := 0, 0, 0
	tokens := []string{}
	callback := quotaCallback(func(ctx context.Context, method string, request, response any) error {
		switch method {
		case "host.http.operation_open":
			opened++
			return quotaRespond(response, map[string]string{"operation_id": "own-operation"})
		case "host.http.do":
			sent++
			req := request.(quotaHTTPRequest)
			if req.OperationID != "own-operation" || req.Method != http.MethodGet || req.URL != quotaUsageURL || len(req.Headers) != 4 || req.Headers.Get("Chatgpt-Account-Id") != "upstream" || req.Headers.Get("Accept") != "application/json" || req.Headers.Get("User-Agent") != "codex-window-refresh-v2" {
				t.Fatalf("unbound host quota query: %+v", req)
			}
			tokens = append(tokens, req.Headers.Get("Authorization"))
			return quotaRespond(response, quotaHTTPResponse{http.StatusOK, []byte(`{"rate_limit":{"allowed":true},"private":"discard this field"}`)})
		case "host.http.cancel":
			cancelled++
			return nil
		case "host.auth.get":
			fixture.gets++
			token := "first-token"
			if fixture.gets > 2 {
				token = "refreshed-token"
			}
			return quotaRespond(response, map[string]any{"auth_index": "i", "json": map[string]string{"type": "codex", "account_id": "upstream", "access_token": token}})
		default:
			return fixture.Call(ctx, method, request, response)
		}
	})
	cpa := &CPA{Host: callback}
	for range 2 {
		raw, err := cpa.Query(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: "available"})
		if err != nil || !json.Valid(raw) || strings.Contains(string(raw), "token") || strings.Contains(string(raw), "private") {
			t.Fatalf("invalid durable quota evidence: %s, %v", raw, err)
		}
	}
	if opened != 2 || sent != 2 || cancelled != 2 || fixture.gets != 4 || !reflect.DeepEqual(tokens, []string{"Bearer first-token", "Bearer refreshed-token"}) || len(cpa.transports) != 0 {
		t.Fatalf("host policy bypass or credentials retained: open=%d sent=%d cancel=%d gets=%d tokens=%v pools=%d", opened, sent, cancelled, fixture.gets, tokens, len(cpa.transports))
	}
}

func TestHostQuotaCancellationStopsOwnedRequestAndJoinsWatcher(t *testing.T) {
	entered, stopped, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var cancels atomic.Int32
	var stopOnce sync.Once
	callback := quotaCallback(func(ctx context.Context, method string, request, response any) error {
		switch method {
		case "host.http.operation_open":
			return quotaRespond(response, map[string]string{"operation_id": "owned-request"})
		case "host.http.do":
			close(entered)
			<-stopped
			return errors.New("private upstream details")
		case "host.http.cancel":
			if request.(map[string]string)["operation_id"] != "owned-request" || ctx.Err() != nil {
				t.Error("cancellation lost handle or inherited expired context")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("cleanup callback has no deadline")
			}
			cancels.Add(1)
			stopOnce.Do(func() { close(stopped) })
			return nil
		default:
			t.Errorf("unexpected callback %s", method)
			return errors.New("unexpected callback")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := (&CPA{Host: callback}).hostQuotaHTTP(ctx, http.Header{})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || cancels.Load() != 1 {
			t.Fatalf("physical request was not cancelled exactly once: %v, %d", err, cancels.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("query returned before cancellation joined or failed to cancel")
	}
}

func TestHostQuotaCleansLateOpenWithoutSendingAndRedactsFailures(t *testing.T) {
	for _, scenario := range []string{"cancel during open", "open returned error and handle", "missing operation", "invalid operation", "do failed", "invalid HTTP status"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var sent, cancelled int
			callback := quotaCallback(func(callCtx context.Context, method string, request, response any) error {
				switch method {
				case "host.http.operation_open":
					if scenario == "missing operation" {
						return nil
					}
					handle := "owned-request"
					if scenario == "invalid operation" {
						handle = "invalid\noperation"
					}
					if err := quotaRespond(response, map[string]string{"operation_id": handle}); err != nil {
						return err
					}
					if scenario == "cancel during open" {
						cancel()
						if callCtx.Err() != nil {
							t.Fatal("opening lost its response handle on caller cancellation")
						}
					}
					if scenario == "open returned error and handle" {
						return errors.New("private file/token/error")
					}
					return nil
				case "host.http.do":
					sent++
					if scenario == "do failed" {
						return errors.New("private proxy password")
					}
					return quotaRespond(response, quotaHTTPResponse{0, []byte("private error")})
				case "host.http.cancel":
					cancelled++
					if request.(map[string]string)["operation_id"] != "owned-request" {
						t.Fatal("cancelled unowned operation")
					}
					return nil
				default:
					t.Fatalf("unexpected callback: %s", method)
					return nil
				}
			})
			response, err := (&CPA{Host: callback}).hostQuotaHTTP(ctx, http.Header{})
			want := "quota_transport_failed"
			if scenario == "cancel during open" {
				want = context.Canceled.Error()
			}
			if err == nil || err.Error() != want || len(response.Body) != 0 {
				t.Fatalf("unexpected failure response: %+v, %v", response, err)
			}
			wantSent, wantCancelled := 0, 0
			if scenario == "do failed" || scenario == "invalid HTTP status" {
				wantSent = 1
			}
			if scenario == "cancel during open" || scenario == "open returned error and handle" || scenario == "do failed" || scenario == "invalid HTTP status" {
				wantCancelled = 1
			}
			if sent != wantSent || cancelled != wantCancelled {
				t.Fatalf("unexpected callbacks: sent=%d cancel=%d", sent, cancelled)
			}
		})
	}
}

func TestHostQuotaResponsesKeepBindingAndBodyBoundaries(t *testing.T) {
	for _, scenario := range []string{"oversized", "replaced account", "forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := &fixtureHost{}
			callback := quotaCallback(func(ctx context.Context, method string, request, response any) error {
				switch method {
				case "host.http.operation_open":
					return quotaRespond(response, map[string]string{"operation_id": "query"})
				case "host.http.cancel":
					return nil
				case "host.http.do":
					value := quotaHTTPResponse{200, []byte(`{"rate_limit":{"allowed":true}}`)}
					switch scenario {
					case "oversized":
						value.Body = make([]byte, maxQuotaBody+1)
					case "replaced account":
						fixture.changed = true
					case "forbidden":
						value.StatusCode = http.StatusForbidden
						value.Body = []byte("private upstream error")
					}
					return quotaRespond(response, value)
				default:
					return fixture.Call(ctx, method, request, response)
				}
			})
			_, err := (&CPA{Host: callback}).Query(context.Background(), store.Account{ID: "a", AuthIndex: "i", Status: "available"})
			want := map[string]string{"oversized": "quota_response_too_large", "replaced account": "account_binding_changed", "forbidden": "quota_upstream_forbidden"}[scenario]
			if err == nil || err.Error() != want {
				t.Fatalf("quota fence failed: %v", err)
			}
		})
	}
}

func TestHostQuotaSuccessfulReadRequiresReleasedOperation(t *testing.T) {
	for _, scenario := range []string{"released", "already closed", "cleanup failed"} {
		t.Run(scenario, func(t *testing.T) {
			cancelled := 0
			callback := quotaCallback(func(_ context.Context, method string, _, response any) error {
				switch method {
				case "host.http.operation_open":
					return quotaRespond(response, map[string]string{"operation_id": "query"})
				case "host.http.do":
					return quotaRespond(response, quotaHTTPResponse{200, []byte(`{"rate_limit":{"allowed":true}}`)})
				case "host.http.cancel":
					cancelled++
					if scenario == "already closed" {
						return errors.New("quota_operation_unavailable")
					}
					if scenario == "cleanup failed" {
						return errors.New("private cleanup error")
					}
					return nil
				default:
					t.Fatalf("unexpected callback: %s", method)
					return nil
				}
			})
			response, err := (&CPA{Host: callback}).hostQuotaHTTP(context.Background(), http.Header{})
			if cancelled != 1 {
				t.Fatal("successful read left native slot occupied", cancelled)
			}
			if scenario == "cleanup failed" {
				if err == nil || err.Error() != "quota_transport_failed" || len(response.Body) != 0 {
					t.Fatal("cleanup failure ignored or leaked", err)
				}
			} else if err != nil || response.StatusCode != 200 {
				t.Fatal("released operation lost successful response", err)
			}
		})
	}
}

type quotaInstanceHost struct {
	quotaCallback
	instance string
}

func (host *quotaInstanceHost) Instance() string { return host.instance }

type quotaPinnedHost struct {
	*quotaInstanceHost
	instances []string
}

func (host *quotaPinnedHost) CallForInstance(ctx context.Context, instance, method string, request, response any) error {
	host.instances = append(host.instances, instance)
	return host.quotaCallback(ctx, method, request, response)
}

func TestHostQuotaCleanupPinsOriginalBridgeAfterHandshakeChanges(t *testing.T) {
	host := &quotaPinnedHost{quotaInstanceHost: &quotaInstanceHost{instance: "original"}}
	host.quotaCallback = func(_ context.Context, method string, _, response any) error {
		switch method {
		case "host.http.operation_open":
			return quotaRespond(response, map[string]string{"operation_id": "original-query"})
		case "host.http.do":
			host.instance = "replacement"
			return quotaRespond(response, quotaHTTPResponse{200, []byte(`{"rate_limit":{"allowed":true}}`)})
		case "host.http.cancel":
			return nil
		default:
			t.Fatalf("unexpected callback: %s", method)
			return nil
		}
	}
	response, err := (&CPA{Host: host}).hostQuotaHTTP(context.Background(), http.Header{})
	if err == nil || err.Error() != "quota_transport_failed" || len(response.Body) != 0 || !reflect.DeepEqual(host.instances, []string{"original", "original", "original"}) {
		t.Fatalf("operation was repinned to new bridge: %v %+v %v", err, response, host.instances)
	}
}

func TestHostQuotaRejectsBridgeReplacementWithoutReusingHandle(t *testing.T) {
	for _, changedAt := range []string{"host.http.operation_open", "host.http.do"} {
		t.Run(changedAt, func(t *testing.T) {
			var sent, cancelled int
			host := &quotaInstanceHost{instance: "original"}
			host.quotaCallback = func(_ context.Context, method string, request, response any) error {
				switch method {
				case "host.http.operation_open":
					if method == changedAt {
						host.instance = "replacement"
					}
					return quotaRespond(response, map[string]string{"operation_id": "original-query"})
				case "host.http.do":
					sent++
					host.instance = "replacement"
					return quotaRespond(response, quotaHTTPResponse{200, []byte(`{"rate_limit":{"allowed":true}}`)})
				case "host.http.cancel":
					cancelled++
					if request.(map[string]string)["operation_id"] != "original-query" {
						t.Fatal("cancelled another host's request")
					}
					return nil
				default:
					t.Fatalf("unexpected callback: %s", method)
					return nil
				}
			}
			response, err := (&CPA{Host: host}).hostQuotaHTTP(context.Background(), http.Header{})
			if err == nil || err.Error() != "quota_transport_failed" || cancelled != 0 || changedAt == "host.http.operation_open" && sent != 0 || len(response.Body) != 0 {
				t.Fatalf("bridge replacement crossed operation fence: %+v %v, sent=%d cancelled=%d", response, err, sent, cancelled)
			}
		})
	}
}

func TestExplicitAccountProxyAndDirectKeepPrivateTransport(t *testing.T) {
	for _, proxy := range []string{"direct", "none", "http://user:private@proxy.example:8080", "https://proxy.example:443", "socks5://proxy.example:1080", "socks5h://proxy.example:1080"} {
		t.Run(proxy, func(t *testing.T) {
			called := 0
			cpa := &CPA{Host: quotaCallback(func(context.Context, string, any, any) error {
				t.Fatal("explicit proxy invoked host HTTP")
				return nil
			}), transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				called++
				if req.URL.String() != quotaUsageURL || req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("Chatgpt-Account-Id") != "test-account" {
					t.Fatal("private query lost account binding")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
			})}
			response, err := cpa.quotaHTTP(context.Background(), credential{Proxy: proxy, Token: "test-token", Upstream: "test-account"})
			if err != nil || response.StatusCode != 200 || called != 1 {
				t.Fatalf("explicit proxy query failed: %+v %v", response, err)
			}
		})
	}
	t.Setenv("HTTPS_PROXY", "http://environment-must-not-be-used.invalid:80")
	for _, proxy := range []string{"direct", "none"} {
		cpa := &CPA{}
		normalized, err := accountQuotaProxy(credential{Proxy: proxy})
		if err != nil {
			t.Fatal(err)
		}
		client := cpa.quotaClient(normalized)
		if client.Transport.(*http.Transport).Proxy != nil {
			t.Fatal("explicit direct inherited an environment proxy")
		}
		cpa.Close()
	}
}

func TestAccountQuotaProxyRejectsAmbiguousOrInvalidDestinations(t *testing.T) {
	for _, cred := range []credential{
		{Proxy: "direct", ProxyDash: "http://private:secret@proxy.example"},
		{Proxy: "http://private:secret@proxy.example?token=secret"},
		{Proxy: "http://proxy.example?"},
		{Proxy: "http://proxy.example:99999"},
		{Proxy: "http://proxy.example:0"},
		{Proxy: "http://proxy.example/path"},
		{Proxy: "file:///private/path"},
	} {
		if proxy, err := accountQuotaProxy(cred); err == nil || err.Error() != "quota_proxy_invalid" || proxy != "" {
			t.Fatal("invalid proxy accepted or error contains credentials")
		}
	}
	for _, cred := range []credential{{}, {Proxy: " NONE ", ProxyCamel: "direct"}, {Proxy: "http://PROXY.example/", ProxyDash: "http://proxy.example"}} {
		if _, err := accountQuotaProxy(cred); err != nil {
			t.Fatal("equivalent proxy aliases rejected", err)
		}
	}
}
