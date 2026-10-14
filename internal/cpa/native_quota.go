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
	"sync"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
const maxQuotaBody = 2 << 20
const maxProxyTransports = 16
const proxyTransportIdleTimeout = 90 * time.Second

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
	connectionMu      sync.RWMutex
	connectionStatus  string
	proxyMu           sync.Mutex
	proxyTransports   map[string]*quotaProxyTransport
	closed            bool
}

type quotaProxyTransport struct {
	transport http.RoundTripper
	lastUsed  time.Time
	users     int
	retired   bool
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

func (r *NativeReader) Read(ctx context.Context, binding AuthBinding) (body []byte, err error) {
	started := time.Now()
	defer func() { err = measuredQuotaError(err, started, "credential") }()
	if !r.Available() || binding.AuthIndex == "" || binding.UpstreamAccountID == "" {
		return nil, ErrBindingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, quotaRequestError(ctx, err, ErrQuotaNativeRead, "credential")
	}
	var raw authGet
	defer func() { clear(raw.JSON) }()
	if err := r.Callback.Call(ctx, "host.auth.get", authRequest{binding.AuthIndex}, &raw); err != nil {
		return nil, quotaRequestError(ctx, err, ErrBindingUnavailable, "credential")
	}
	var credential struct {
		Type        string `json:"type"`
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
		Expired     string `json:"expired"`
		ProxyURL    string `json:"proxy_url"`
		ProxyCamel  string `json:"proxyUrl"`
		ProxyDash   string `json:"proxy-url"`
		ProxyUpper  string `json:"proxyURL"`
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
	proxy := ""
	for _, value := range []string{credential.ProxyURL, credential.ProxyCamel, credential.ProxyDash, credential.ProxyUpper} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if proxy != "" && proxy != value {
			return nil, quotaError(ErrQuotaProxyInvalid, "proxy", 0)
		}
		proxy = value
	}
	headers := quotaRequestHeaders(token, binding.UpstreamAccountID)
	var response quotaHTTPResponse
	if proxy == "" {
		response, err = r.hostHTTP(ctx, headers)
	} else {
		response, err = r.proxyHTTP(ctx, proxy, headers)
	}
	if err != nil {
		clear(response.Body)
		return nil, err
	}
	if err = quotaUpstreamError(response.StatusCode); err != nil {
		clear(response.Body)
		return nil, err
	}
	if len(response.Body) > maxQuotaBody {
		clear(response.Body)
		return nil, quotaError(ErrQuotaResponseTooLarge, "parse", response.StatusCode)
	}
	if !json.Valid(response.Body) {
		clear(response.Body)
		return nil, quotaError(ErrQuotaResponseInvalid, "parse", response.StatusCode)
	}
	return response.Body, nil
}

func (r *NativeReader) hostHTTP(ctx context.Context, headers http.Header) (quotaHTTPResponse, error) {
	if ctx.Err() != nil {
		return quotaHTTPResponse{}, quotaRequestError(ctx, ctx.Err(), ErrQuotaHostHTTP, "host_http")
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	// Opening is local bookkeeping. Preserve the returned handle even if the
	// query deadline expires during the RPC, so it can still be cancelled.
	if err := r.Callback.Call(context.WithoutCancel(ctx), "host.http.operation_open", struct{}{}, &operation); err != nil || operation.ID == "" {
		if operation.ID != "" {
			_ = r.cancelOperation(operation.ID)
		}
		r.setConnectionStatus("operation_open_failed")
		return quotaHTTPResponse{}, quotaRequestError(ctx, err, ErrQuotaHostOperation, "operation_open")
	}
	r.connectionMu.Lock()
	if r.connectionStatus != "verified" && r.connectionStatus != "operation_cancel_failed" {
		r.connectionStatus = "operation_open_verified"
	}
	r.connectionMu.Unlock()
	finished, cancelled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(cancelled)
		select {
		case <-ctx.Done():
			_ = r.cancelOperation(operation.ID)
		case <-finished:
		}
	}()
	var response quotaHTTPResponse
	err := r.Callback.Call(ctx, "host.http.do", quotaHTTPRequest{operation.ID, http.MethodGet, codexUsageURL, headers}, &response)
	close(finished)
	<-cancelled
	if err != nil || ctx.Err() != nil {
		_ = r.cancelOperation(operation.ID)
		clear(response.Body)
		return quotaHTTPResponse{}, quotaRequestError(ctx, err, ErrQuotaHostHTTP, "host_http")
	}
	return response, nil
}

func nativeProxyTransport(proxy string) (http.RoundTripper, error) {
	normalized, err := normalizeQuotaProxy(proxy)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		transport = base.Clone()
	}
	transport.Proxy = nil // An explicit account proxy must never inherit env proxies.
	transport.DisableKeepAlives = false
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = proxyTransportIdleTimeout
	transport.TLSHandshakeTimeout = 10 * time.Second
	if normalized == "direct" {
		return transport, nil
	}
	parsed, _ := url.Parse(normalized)
	transport.Proxy = http.ProxyURL(parsed)
	return transport, nil
}

func normalizeQuotaProxy(proxy string) (string, error) {
	proxy = strings.TrimSpace(proxy)
	if strings.EqualFold(proxy, "direct") || strings.EqualFold(proxy, "none") {
		return "direct", nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", ErrQuotaProxyInvalid
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", ErrQuotaProxyInvalid
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrQuotaProxyInvalid
		}
	}
	// DNS names and IP literals are case insensitive; an IPv6 zone names a
	// local interface and must retain its case on case-sensitive platforms.
	if zone := strings.IndexByte(parsed.Host, '%'); zone >= 0 {
		parsed.Host = strings.ToLower(parsed.Host[:zone]) + parsed.Host[zone:]
	} else {
		parsed.Host = strings.ToLower(parsed.Host)
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}

func (r *NativeReader) proxyHTTP(ctx context.Context, proxy string, headers http.Header) (quotaHTTPResponse, error) {
	transport, release, err := r.acquireProxyTransport(proxy)
	if err != nil {
		return quotaHTTPResponse{}, quotaError(ErrQuotaProxyInvalid, "proxy", 0)
	}
	defer release()
	client := &http.Client{Transport: transport, Timeout: maxQuotaRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return quotaHTTPResponse{}, quotaError(ErrQuotaNativeRead, "proxy_http", 0)
	}
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return quotaHTTPResponse{}, quotaRequestError(ctx, err, ErrQuotaNativeRead, "proxy_http")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
	if err != nil {
		clear(body)
		return quotaHTTPResponse{}, quotaRequestError(ctx, err, ErrQuotaNativeRead, "proxy_http")
	}
	return quotaHTTPResponse{resp.StatusCode, body}, nil
}

func closeQuotaTransport(transport http.RoundTripper) {
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// The pool belongs to this reader. Keys include the complete proxy identity,
// including proxy credentials, but are never returned in diagnostics or logs.
func (r *NativeReader) acquireProxyTransport(proxy string) (http.RoundTripper, func(), error) {
	key, err := normalizeQuotaProxy(proxy)
	if err != nil {
		return nil, nil, ErrQuotaProxyInvalid
	}
	r.proxyMu.Lock()
	if r.closed {
		r.proxyMu.Unlock()
		return nil, nil, ErrQuotaNativeRead
	}
	now := time.Now()
	retired := []http.RoundTripper{}
	retire := func(key string, entry *quotaProxyTransport) {
		delete(r.proxyTransports, key)
		entry.retired = true
		if entry.users == 0 {
			retired = append(retired, entry.transport)
		}
	}
	for key, entry := range r.proxyTransports {
		if now.Sub(entry.lastUsed) >= proxyTransportIdleTimeout {
			retire(key, entry)
		}
	}
	entry := r.proxyTransports[key]
	if entry == nil {
		factory := r.TransportForProxy
		if factory == nil {
			factory = nativeProxyTransport
		}
		// Factories only configure transports, so creation is serialized to
		// avoid duplicate pools for simultaneous queries of one proxy.
		transport, factoryErr := factory(proxy)
		if factoryErr != nil || transport == nil {
			r.proxyMu.Unlock()
			if transport != nil {
				closeQuotaTransport(transport)
			}
			for _, transport := range retired {
				closeQuotaTransport(transport)
			}
			return nil, nil, ErrQuotaProxyInvalid
		}
		if len(r.proxyTransports) >= maxProxyTransports {
			var oldestKey string
			var oldest *quotaProxyTransport
			for key, candidate := range r.proxyTransports {
				if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
					oldestKey, oldest = key, candidate
				}
			}
			retire(oldestKey, oldest)
		}
		entry = &quotaProxyTransport{transport: transport}
		if r.proxyTransports == nil {
			r.proxyTransports = map[string]*quotaProxyTransport{}
		}
		r.proxyTransports[key] = entry
	}
	entry.users++
	entry.lastUsed = now
	r.proxyMu.Unlock()
	for _, transport := range retired {
		closeQuotaTransport(transport)
	}
	return entry.transport, func() {
		r.proxyMu.Lock()
		entry.users--
		entry.lastUsed = time.Now()
		closeIdle := entry.retired && entry.users == 0
		r.proxyMu.Unlock()
		if closeIdle {
			closeQuotaTransport(entry.transport)
		}
	}, nil
}

// Close releases idle proxy connections. Active requests retain their context
// and release their transport after reading the response.
func (r *NativeReader) Close() error {
	r.proxyMu.Lock()
	r.closed = true
	retired := []http.RoundTripper{}
	for _, entry := range r.proxyTransports {
		entry.retired = true
		if entry.users == 0 {
			retired = append(retired, entry.transport)
		}
	}
	r.proxyTransports = nil
	r.proxyMu.Unlock()
	for _, transport := range retired {
		closeQuotaTransport(transport)
	}
	return nil
}

func (r *NativeReader) setConnectionStatus(status string) {
	r.connectionMu.Lock()
	r.connectionStatus = status
	r.connectionMu.Unlock()
}

// ConnectionStatus is a snapshot. It never probes the host or upstream network.
func (r *NativeReader) ConnectionStatus() string {
	if !r.Available() {
		return "callback_missing"
	}
	r.connectionMu.RLock()
	defer r.connectionMu.RUnlock()
	if r.connectionStatus == "" {
		return "unchecked"
	}
	return r.connectionStatus
}

func (r *NativeReader) cancelOperation(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := r.Callback.Call(ctx, "host.http.cancel", map[string]string{"operation_id": id}, &struct{}{})
	if err != nil {
		r.setConnectionStatus("operation_cancel_failed")
	}
	return err
}

// CheckConnection only creates/cancels local host bookkeeping. It does not
// read credentials, issue HTTP, or execute a model request.
func (r *NativeReader) CheckConnection(ctx context.Context) (err error) {
	started := time.Now()
	defer func() { err = measuredQuotaError(err, started, "operation_open") }()
	if !r.Available() {
		return quotaError(ErrQuotaHostOperation, "operation_open", 0)
	}
	if ctx.Err() != nil {
		return quotaRequestError(ctx, ctx.Err(), ErrQuotaHostOperation, "operation_open")
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	err = r.Callback.Call(context.WithoutCancel(ctx), "host.http.operation_open", struct{}{}, &operation)
	var cleanupErr error
	if operation.ID != "" {
		cleanupErr = r.cancelOperation(operation.ID)
	}
	if err != nil || operation.ID == "" {
		r.setConnectionStatus("operation_open_failed")
		return quotaRequestError(ctx, err, ErrQuotaHostOperation, "operation_open")
	}
	if cleanupErr != nil {
		r.setConnectionStatus("operation_cancel_failed")
		return quotaError(ErrQuotaHostOperation, "operation_cancel", 0)
	}
	r.setConnectionStatus("verified")
	if ctx.Err() != nil {
		return quotaRequestError(ctx, ctx.Err(), ErrQuotaHostOperation, "operation_open")
	}
	return nil
}
