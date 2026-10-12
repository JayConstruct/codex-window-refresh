package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
const maxQuotaBody = 2 << 20

var ErrQuotaCredentialMissing = errors.New("quota_credential_missing")
var ErrQuotaCredentialExpired = errors.New("quota_credential_expired")
var ErrQuotaProxyInvalid = errors.New("quota_proxy_invalid")
var ErrQuotaHostHTTP = errors.New("quota_host_http_failed")
var ErrQuotaNativeRead = errors.New("quota_native_query_failed")

// NativeReader queries using host-owned credentials. Tokens are read afresh for
// each query and never retained on the reader, stored, or included in errors.
// CPA remains the sole OAuth refresh owner; this reader never rotates grants.
type NativeReader struct {
	Callback Callback
	// TransportForProxy is a test seam; production builds a private transport.
	TransportForProxy func(string) (http.RoundTripper, error)
}

func (r *NativeReader) Available() bool { return r != nil && r.Callback != nil }

type quotaHTTPRequest struct {
	OperationID string      `json:"operation_id,omitempty"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	Headers     http.Header `json:"headers"`
}
type quotaHTTPResponse struct {
	StatusCode int
	Body       []byte
}

func (r *NativeReader) Read(ctx context.Context, binding AuthBinding) ([]byte, error) {
	if !r.Available() || binding.AuthIndex == "" || binding.UpstreamAccountID == "" {
		return nil, ErrBindingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw authGet
	defer func() { clear(raw.JSON) }()
	if err := r.Callback.Call(ctx, "host.auth.get", authRequest{binding.AuthIndex}, &raw); err != nil {
		return nil, ErrBindingUnavailable
	}
	var credential struct {
		Type        string `json:"type"`
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
		Expired     string `json:"expired"`
		ProxyURL    string `json:"proxy_url"`
		ProxyCamel  string `json:"proxyUrl"`
	}
	if len(raw.JSON) > 1<<20 || json.Unmarshal(raw.JSON, &credential) != nil || raw.AuthIndex != binding.AuthIndex || credential.Type != "codex" || credential.AccountID != binding.UpstreamAccountID || generation(string(binding.AccountID), credential.AccountID) != binding.Generation {
		return nil, ErrBindingChanged
	}
	token := strings.TrimSpace(credential.AccessToken)
	if token == "" {
		token = strings.TrimSpace(credential.Token)
	}
	if token == "" || len(token) > 65536 || strings.IndexFunc(token, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
		return nil, ErrQuotaCredentialMissing
	}
	if credential.Expired != "" {
		expires, err := time.Parse(time.RFC3339, credential.Expired)
		if err != nil {
			return nil, ErrQuotaCredentialMissing
		}
		if !expires.After(time.Now()) {
			return nil, ErrQuotaCredentialExpired
		}
	}
	proxy := strings.TrimSpace(credential.ProxyURL)
	if proxy == "" {
		proxy = strings.TrimSpace(credential.ProxyCamel)
	} else if credential.ProxyCamel != "" && strings.TrimSpace(credential.ProxyCamel) != proxy {
		return nil, ErrQuotaProxyInvalid
	}
	headers := http.Header{"Authorization": {"Bearer " + token}, "Chatgpt-Account-Id": {binding.UpstreamAccountID}, "Accept": {"application/json"}}
	var response quotaHTTPResponse
	var err error
	if proxy == "" {
		response, err = r.hostHTTP(ctx, headers)
	} else {
		response, err = r.proxyHTTP(ctx, proxy, headers)
	}
	if err != nil {
		clear(response.Body)
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		clear(response.Body)
		return nil, ErrQuotaCredentialExpired
	}
	if response.StatusCode != http.StatusOK || len(response.Body) > maxQuotaBody || !json.Valid(response.Body) {
		clear(response.Body)
		return nil, ErrQuotaNativeRead
	}
	return response.Body, nil
}

func (r *NativeReader) hostHTTP(ctx context.Context, headers http.Header) (quotaHTTPResponse, error) {
	if ctx.Err() != nil {
		return quotaHTTPResponse{}, ErrQuotaHostHTTP
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	// Opening is local bookkeeping. Preserve the returned handle even if the
	// query deadline expires during the RPC, so it can still be cancelled.
	if err := r.Callback.Call(context.WithoutCancel(ctx), "host.http.operation.open", struct{}{}, &operation); err != nil || operation.ID == "" {
		return quotaHTTPResponse{}, ErrQuotaHostHTTP
	}
	finished, cancelled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(cancelled)
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = r.Callback.Call(cancelCtx, "host.http.cancel", map[string]string{"operation_id": operation.ID}, &struct{}{})
		case <-finished:
		}
	}()
	var response quotaHTTPResponse
	err := r.Callback.Call(ctx, "host.http.do", quotaHTTPRequest{operation.ID, http.MethodGet, codexUsageURL, headers}, &response)
	close(finished)
	<-cancelled
	if err != nil || ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = r.Callback.Call(cleanup, "host.http.cancel", map[string]string{"operation_id": operation.ID}, &struct{}{})
		cancel()
		clear(response.Body)
		return quotaHTTPResponse{}, ErrQuotaHostHTTP
	}
	return response, nil
}

func nativeProxyTransport(proxy string) (http.RoundTripper, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // An explicit account proxy must never inherit env proxies.
	transport.DisableKeepAlives = true
	transport.TLSHandshakeTimeout = 10 * time.Second
	if strings.EqualFold(proxy, "direct") || strings.EqualFold(proxy, "none") {
		return transport, nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrQuotaProxyInvalid
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, ErrQuotaProxyInvalid
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrQuotaProxyInvalid
		}
	}
	transport.Proxy = http.ProxyURL(parsed)
	return transport, nil
}

func (r *NativeReader) proxyHTTP(ctx context.Context, proxy string, headers http.Header) (quotaHTTPResponse, error) {
	factory := r.TransportForProxy
	if factory == nil {
		factory = nativeProxyTransport
	}
	transport, err := factory(proxy)
	if err != nil {
		return quotaHTTPResponse{}, ErrQuotaProxyInvalid
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return quotaHTTPResponse{}, ErrQuotaNativeRead
	}
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return quotaHTTPResponse{}, ErrQuotaNativeRead
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
	if err != nil {
		clear(body)
		return quotaHTTPResponse{}, ErrQuotaNativeRead
	}
	return quotaHTTPResponse{resp.StatusCode, body}, nil
}
