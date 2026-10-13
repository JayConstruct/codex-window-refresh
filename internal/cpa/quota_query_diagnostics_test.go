package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func assertQuotaDiagnostic(t *testing.T, err, want error, stage string, status int) {
	t.Helper()
	var public interface{ QuotaDiagnostic() core.QuotaDiagnostic }
	if !errors.Is(err, want) || !errors.As(err, &public) {
		t.Fatalf("wrong public error: %v", err)
	}
	d := public.QuotaDiagnostic()
	raw, _ := json.Marshal(d)
	if d.Code != want.Error() || d.Stage != stage || d.HTTPStatus != status || d.ElapsedMS < 0 || strings.Contains(string(raw), "private") || strings.Contains(err.Error(), "private") {
		t.Fatalf("wrong/sensitive diagnosis: %s", raw)
	}
}

func TestNativeQuotaDistinguishesOperationFailureBeforeAnyHTTPRequest(t *testing.T) {
	cb := callbackFunc(func(_ context.Context, method string, _, out any) error {
		switch method {
		case "host.auth.get":
			return respond(out, authGet{AuthIndex: "auth-index", JSON: json.RawMessage(`{"type":"codex","account_id":"real","access_token":"private"}`)})
		case "host.http.operation_open":
			return errors.New("unsupported callback with private detail")
		default:
			t.Fatal("failed operation issued an HTTP/model request")
			return nil
		}
	})
	r := &NativeReader{Callback: cb}
	_, err := r.Read(context.Background(), nativeTestBinding())
	assertQuotaDiagnostic(t, err, ErrQuotaHostOperation, "operation_open", 0)
	if r.ConnectionStatus() != "operation_open_failed" {
		t.Fatal("operation failure was not published")
	}
}

func TestNativeConnectionCheckIsLocalAndAlwaysReleasesReturnedHandle(t *testing.T) {
	for _, mode := range []string{"success", "cancel fails", "open fails with handle", "deadline during open"} {
		t.Run(mode, func(t *testing.T) {
			opened, cancelled := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &NativeReader{Callback: callbackFunc(func(_ context.Context, method string, req, out any) error {
				switch method {
				case "host.http.operation_open":
					opened++
					if err := respond(out, map[string]string{"operation_id": "local-op"}); err != nil {
						return err
					}
					if mode == "deadline during open" {
						cancel()
					}
					if mode == "open fails with handle" {
						return errors.New("private callback failure")
					}
				case "host.http.cancel":
					cancelled++
					if req.(map[string]string)["operation_id"] != "local-op" {
						t.Fatal("wrong cleanup handle")
					}
					if mode == "cancel fails" {
						return errors.New("private cancel failure")
					}
				default:
					t.Fatal("local check touched credentials, HTTP or model callbacks")
				}
				return nil
			})}
			if r.ConnectionStatus() != "unchecked" {
				t.Fatal("new connection claimed validation")
			}
			err := r.CheckConnection(ctx)
			if opened != 1 || cancelled != 1 {
				t.Fatal("local probe did not release handle exactly once")
			}
			switch mode {
			case "success":
				if err != nil || r.ConnectionStatus() != "verified" {
					t.Fatal("local check failed")
				}
			case "cancel fails":
				assertQuotaDiagnostic(t, err, ErrQuotaHostOperation, "operation_cancel", 0)
			case "open fails with handle":
				assertQuotaDiagnostic(t, err, ErrQuotaHostOperation, "operation_open", 0)
			case "deadline during open":
				assertQuotaDiagnostic(t, err, ErrQuotaCancelled, "operation_open", 0)
			}
		})
	}
}

func TestNativeQuotaProxyAliasesCannotSilentlyUseGlobalProxy(t *testing.T) {
	for _, key := range []string{"proxy_url", "proxyUrl", "proxy-url", "proxyURL"} {
		t.Run(key, func(t *testing.T) {
			credential, _ := json.Marshal(map[string]string{"type": "codex", "account_id": "real", "access_token": "private", key: "direct"})
			cb := callbackFunc(func(_ context.Context, method string, _, out any) error {
				if method != "host.auth.get" {
					t.Fatal("proxy alias fell through to host/global transport")
				}
				return respond(out, authGet{AuthIndex: "auth-index", JSON: credential})
			})
			reads := 0
			r := &NativeReader{Callback: cb, TransportForProxy: func(proxy string) (http.RoundTripper, error) {
				if proxy != "direct" {
					t.Fatal("wrong alias value")
				}
				return roundTripFunc(func(req *http.Request) (*http.Response, error) {
					reads++
					if req.Header.Get("User-Agent") != codexQuotaUserAgent {
						t.Fatal("missing quota request identity")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
				}), nil
			}}
			if _, err := r.Read(context.Background(), nativeTestBinding()); err != nil || reads != 1 {
				t.Fatal("proxy alias query failed")
			}
		})
	}
	cb := callbackFunc(func(_ context.Context, method string, _, out any) error {
		if method != "host.auth.get" {
			t.Fatal("conflicting proxy sent a request")
		}
		return respond(out, authGet{AuthIndex: "auth-index", JSON: json.RawMessage(`{"type":"codex","account_id":"real","access_token":"private","proxy_url":"direct","proxyURL":"http://private:private@proxy.invalid"}`)})
	})
	_, err := (&NativeReader{Callback: cb}).Read(context.Background(), nativeTestBinding())
	assertQuotaDiagnostic(t, err, ErrQuotaProxyInvalid, "proxy", 0)
}

func TestBridgeSeparatesManagementHTTPFromUpstreamHTTPAndMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		management int
		payload    string
		want       error
		stage      string
		status     int
	}{
		{"management authentication", 401, `private`, ErrBridgeAuthentication, "management_http", 401},
		{"management forbidden", 403, `private`, ErrBridgeAuthentication, "management_http", 403},
		{"management route", 404, `private`, ErrBridgeEndpoint, "management_http", 404},
		{"management gateway", 502, `private`, ErrBridgeHTTP, "management_http", 502},
		{"upstream authentication", 200, `{"status_code":401,"body":"private"}`, ErrQuotaCredentialExpired, "upstream_http", 401},
		{"upstream forbidden", 200, `{"status_code":403,"body":"private"}`, ErrQuotaForbidden, "upstream_http", 403},
		{"upstream rate limit", 200, `{"status_code":429,"body":"private"}`, ErrQuotaRateLimited, "upstream_http", 429},
		{"upstream server", 200, `{"status_code":503,"body":"private"}`, ErrQuotaUpstreamServer, "upstream_http", 503},
		{"upstream redirect", 200, `{"status_code":302,"body":"private"}`, ErrQuotaUpstreamHTTP, "upstream_http", 302},
		{"missing status", 200, `{"body":"{}"}`, ErrQuotaResponseInvalid, "management_parse", 200},
		{"conflicting status", 200, `{"status_code":200,"statusCode":401,"body":"{}"}`, ErrQuotaResponseInvalid, "management_parse", 200},
		{"invalid envelope", 200, `private`, ErrQuotaResponseInvalid, "management_parse", 200},
		{"invalid upstream body", 200, `{"status_code":200,"body":"private"}`, ErrQuotaResponseInvalid, "parse", 200},
		{"too large", 200, strings.Repeat("x", maxQuotaBody+1), ErrQuotaResponseTooLarge, "management_parse", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := false
			r := &BridgeReader{ManagementURL: "http://management.invalid", ManagementKeyEnv: "KEY", Getenv: func(string) string { return "private" }, Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Header map[string]string `json:"header"`
				}
				if json.NewDecoder(req.Body).Decode(&payload) != nil || payload.Header["User-Agent"] != codexQuotaUserAgent || payload.Header["Authorization"] != "Bearer $TOKEN$" {
					t.Fatal("bridge lost request identity or token substitution")
				}
				return &http.Response{StatusCode: tc.management, Body: &trackingQuotaBody{Reader: strings.NewReader(tc.payload), closed: &closed}}, nil
			})}}
			_, err := r.Read(context.Background(), AuthBinding{AuthIndex: "index", UpstreamAccountID: "account"})
			assertQuotaDiagnostic(t, err, tc.want, tc.stage, tc.status)
			if !closed {
				t.Fatal("bridge response body leaked")
			}
		})
	}
}

type trackingQuotaBody struct {
	io.Reader
	closed *bool
}

func (b *trackingQuotaBody) Close() error { *b.closed = true; return nil }

func TestQuotaTimeoutIsDistinctFromConnectionFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := &BridgeReader{ManagementURL: "http://management.invalid", ManagementKeyEnv: "KEY", Getenv: func(string) string { return "private" }, Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}}
	_, err := r.Read(ctx, AuthBinding{AuthIndex: "index", UpstreamAccountID: "account"})
	assertQuotaDiagnostic(t, err, ErrQuotaTimeout, "management_http", 0)
}
