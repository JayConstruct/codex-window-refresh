package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const quotaUsageURL = "https://chatgpt.com/backend-api/wham/usage"
const maxQuotaBody = 2 << 20

type quotaHTTPRequest struct {
	OperationID string      `json:"operation_id"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	Headers     http.Header `json:"headers"`
}

type quotaHTTPResponse struct {
	StatusCode int
	Body       []byte
}

// An empty proxy means inherit CPA's global transport policy. It must remain
// distinct from explicit direct/none, which opts out of the global proxy.
func accountQuotaProxy(cred credential) (string, error) {
	proxy := ""
	for _, value := range []string{cred.Proxy, cred.ProxyCamel, cred.ProxyDash, cred.ProxyUpper} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.EqualFold(value, "none") || strings.EqualFold(value, "direct") {
			value = "direct"
		} else {
			u, err := url.Parse(value)
			if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
				return "", errors.New("quota_proxy_invalid")
			}
			u.Scheme = strings.ToLower(u.Scheme)
			switch u.Scheme {
			case "http", "https", "socks5", "socks5h":
			default:
				return "", errors.New("quota_proxy_invalid")
			}
			if u.Port() != "" {
				port, err := strconv.Atoi(u.Port())
				if err != nil || port < 1 || port > 65535 {
					return "", errors.New("quota_proxy_invalid")
				}
			}
			// Preserve IPv6 zone names, which can be case sensitive.
			if zone := strings.IndexByte(u.Host, '%'); zone >= 0 {
				u.Host = strings.ToLower(u.Host[:zone]) + u.Host[zone:]
			} else {
				u.Host = strings.ToLower(u.Host)
			}
			u.Path, u.RawPath = "", ""
			value = u.String()
		}
		if proxy != "" && proxy != value {
			return "", errors.New("quota_proxy_invalid")
		}
		proxy = value
	}
	return proxy, nil
}

func (c *CPA) quotaHTTP(ctx context.Context, cred credential) (quotaHTTPResponse, error) {
	proxy, err := accountQuotaProxy(cred)
	if err != nil {
		return quotaHTTPResponse{}, err
	}
	if ctx.Err() != nil {
		return quotaHTTPResponse{}, ctx.Err()
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+cred.Token)
	headers.Set("Chatgpt-Account-Id", cred.Upstream)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "codex-window-refresh-v2")
	if proxy == "" && c.transport == nil && c.endpoint == "" {
		return c.hostQuotaHTTP(ctx, headers)
	}
	// Private test seams can inject a transport or local endpoint. Production
	// enters this path only for an explicitly selected account proxy/direct.
	if proxy == "" {
		proxy = "direct"
	}
	client := c.quotaClient(proxy)
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = quotaUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaHTTPResponse{}, errors.New("quota_request_invalid")
	}
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return quotaHTTPResponse{}, quotaTransportError(ctx)
	}
	defer resp.Body.Close()
	// Error bodies have no quota evidence and can contain private account data.
	if resp.StatusCode != http.StatusOK {
		return quotaHTTPResponse{StatusCode: resp.StatusCode}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
	if err != nil {
		clear(body)
		return quotaHTTPResponse{}, errors.New("quota_body_failed")
	}
	return quotaHTTPResponse{StatusCode: resp.StatusCode, Body: body}, nil
}

func quotaTransportError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("quota_transport_failed")
}

// hostQuotaHTTP uses an operation handle so cancelling the daemon request also
// cancels the physical request in CPA. The hostlink layer owns handle/instance
// association; callbacks must never replay an operation on a replacement host.
func (c *CPA) hostQuotaHTTP(ctx context.Context, headers http.Header) (response quotaHTTPResponse, resultErr error) {
	if c.Host == nil || ctx.Err() != nil {
		return quotaHTTPResponse{}, quotaTransportError(ctx)
	}
	instance := ""
	instanceHost, pinned := c.Host.(interface{ Instance() string })
	if pinned {
		instance = instanceHost.Instance()
		if instance == "" {
			return quotaHTTPResponse{}, errors.New("quota_transport_failed")
		}
	}
	call := func(ctx context.Context, method string, request, response any) error {
		if pinned {
			if caller, ok := c.Host.(interface {
				CallForInstance(context.Context, string, string, any, any) error
			}); ok {
				return caller.CallForInstance(ctx, instance, method, request, response)
			}
			if instanceHost.Instance() != instance {
				return errors.New("bridge_instance_changed")
			}
		}
		return c.Host.Call(ctx, method, request, response)
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	// Opening is local bookkeeping. Keep enough time to obtain the handle even
	// if the query expires while opening, then cancel it without sending HTTP.
	openCtx, openCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err := call(openCtx, "host.http.operation_open", struct{}{}, &operation)
	openCancel()
	validHandle := operation.ID != "" && len(operation.ID) <= 256 && strings.IndexFunc(operation.ID, func(char rune) bool { return char < 33 || char > 126 }) < 0
	var cancelOnce sync.Once
	var cancelErr error
	cancelOperation := func() {
		cancelOnce.Do(func() {
			if validHandle {
				cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				cancelErr = call(cancelCtx, "host.http.cancel", map[string]string{"operation_id": operation.ID}, &struct{}{})
				if cancelErr != nil && cancelErr.Error() == "quota_operation_unavailable" {
					cancelErr = nil // Native cleanup has already released the operation.
				}
			}
		})
	}
	defer func() {
		// The native bridge retains the operation until explicit cancellation,
		// even when the physical host HTTP call has already completed.
		cancelOperation()
		if resultErr == nil && cancelErr != nil {
			clear(response.Body)
			response = quotaHTTPResponse{}
			resultErr = quotaTransportError(ctx)
		}
	}()
	if err != nil || !validHandle || ctx.Err() != nil || pinned && instanceHost.Instance() != instance {
		cancelOperation()
		return quotaHTTPResponse{}, quotaTransportError(ctx)
	}
	finished, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			cancelOperation()
		case <-finished:
		}
	}()
	err = call(ctx, "host.http.do", quotaHTTPRequest{operation.ID, http.MethodGet, quotaUsageURL, headers}, &response)
	close(finished)
	<-joined
	if err != nil || ctx.Err() != nil || pinned && instanceHost.Instance() != instance {
		cancelOperation()
		clear(response.Body)
		return quotaHTTPResponse{}, quotaTransportError(ctx)
	}
	if response.StatusCode < 100 || response.StatusCode > 599 {
		clear(response.Body)
		return quotaHTTPResponse{}, errors.New("quota_transport_failed")
	}
	if response.StatusCode != http.StatusOK {
		clear(response.Body)
		response.Body = nil
	}
	return response, nil
}

func (c *CPA) quotaClient(proxy string) *http.Client {
	transport := c.transport
	if transport == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.transports == nil {
			c.transports = map[string]*http.Transport{}
		}
		t := c.transports[proxy]
		if t == nil {
			t = http.DefaultTransport.(*http.Transport).Clone()
			t.Proxy = nil
			t.MaxIdleConns = 16
			t.MaxIdleConnsPerHost = 4
			t.IdleConnTimeout = 30 * time.Second
			if proxy != "direct" {
				u, _ := url.Parse(proxy) // accountQuotaProxy already validated it.
				t.Proxy = http.ProxyURL(u)
			}
			// Active requests retain their transport; retire only idle connections.
			if len(c.transports) >= 16 {
				for key, old := range c.transports {
					old.CloseIdleConnections()
					delete(c.transports, key)
					break
				}
			}
			c.transports[proxy] = t
		}
		transport = t
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
