package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-window-refresh/internal/v2/quota"
	"codex-window-refresh/internal/v2/store"
)

type Callback interface {
	Call(context.Context, string, any, any) error
}
type Source interface {
	Accounts(context.Context) ([]store.Account, error)
	Query(context.Context, store.Account) (json.RawMessage, error)
}
type CPA struct {
	Host Callback
	// Transport and endpoint are private test seams. Production always uses the
	// fixed quota endpoint and explicitly selected account proxy.
	endpoint   string
	transport  http.RoundTripper
	mu         sync.Mutex
	transports map[string]*http.Transport
}
type authEntry struct {
	ID          string `json:"id"`
	Index       string `json:"auth_index"`
	Provider    string `json:"provider"`
	Type        string `json:"type"`
	Email       string `json:"email"`
	ProjectID   string `json:"project_id"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	RuntimeOnly bool   `json:"runtime_only"`
}

func accountStatus(entry authEntry) string {
	if entry.Disabled || entry.Status == "disabled" {
		return "disabled"
	}
	if entry.Unavailable {
		return "unavailable"
	}
	switch entry.Status {
	case "", "active":
		return "available"
	default:
		return "unavailable"
	}
}

func (c *CPA) Accounts(ctx context.Context) ([]store.Account, error) {
	var list struct {
		Files []authEntry `json:"files"`
	}
	if err := c.Host.Call(ctx, "host.auth.list", struct{}{}, &list); err != nil {
		return nil, err
	}
	if len(list.Files) > 10000 {
		return nil, errors.New("inventory_too_large")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	indexes := map[string]int{}
	for _, a := range list.Files {
		if store.ValidID(a.ID) {
			counts[a.ID]++
		}
		if a.Index != "" {
			indexes[a.Index]++
		}
	}
	accounts := []store.Account{}
	for _, a := range list.Files {
		if a.Provider != "codex" && !(a.Provider == "" && a.Type == "codex") {
			continue
		}
		if !store.ValidID(a.ID) {
			continue
		}
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		status := accountStatus(a)
		if a.Index == "" || a.RuntimeOnly || counts[a.ID] > 1 || indexes[a.Index] > 1 {
			status = "binding_unavailable"
			a.Index = ""
		}
		accounts = append(accounts, store.Account{ID: a.ID, AuthIndex: a.Index, Status: status})
	}
	return accounts, nil
}

type credential struct {
	Type       string `json:"type"`
	Upstream   string `json:"account_id"`
	Token      string `json:"access_token"`
	Fallback   string `json:"token"`
	Proxy      string `json:"proxy_url"`
	ProxyCamel string `json:"proxyUrl"`
	ProxyDash  string `json:"proxy-url"`
	ProxyUpper string `json:"proxyURL"`
	status     string
	statusAt   string
}

func (c *CPA) binding(ctx context.Context, a store.Account) (credential, error) {
	cred, err := c.readBinding(ctx, a)
	if err != nil {
		return credential{}, err
	}
	if !validQuotaToken(cred.Token) {
		return credential{}, errors.New("quota_credential_missing")
	}
	return cred, nil
}

func validQuotaToken(token string) bool {
	return token != "" && len(token) <= 65536 && strings.IndexFunc(token, func(character rune) bool { return character < 33 || character > 126 }) < 0
}
func (c *CPA) readBinding(ctx context.Context, a store.Account) (credential, error) {
	return c.readBindingFor(ctx, a, false)
}

func (c *CPA) readBindingFor(ctx context.Context, a store.Account, requireAvailable bool) (credential, error) {
	var runtime struct {
		Auth authEntry `json:"auth"`
	}
	if err := c.Host.Call(ctx, "host.auth.get_runtime", map[string]string{"auth_index": a.AuthIndex}, &runtime); err != nil {
		return credential{}, err
	}
	r := runtime.Auth
	statusAt := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	status := accountStatus(r)
	if requireAvailable && status == "disabled" {
		return credential{}, errors.New("account_disabled")
	}
	if requireAvailable && status != "available" {
		return credential{}, errors.New("account_identity_unavailable")
	}
	if r.ID != a.ID || r.Index != a.AuthIndex || r.RuntimeOnly || (r.Provider != "codex" && !(r.Provider == "" && r.Type == "codex")) {
		return credential{}, errors.New("account_binding_changed")
	}
	var raw struct {
		Index string          `json:"auth_index"`
		JSON  json.RawMessage `json:"json"`
	}
	defer func() { clear(raw.JSON) }()
	if err := c.Host.Call(ctx, "host.auth.get", map[string]string{"auth_index": a.AuthIndex}, &raw); err != nil {
		return credential{}, err
	}
	var cred credential
	if len(raw.JSON) > 1<<20 || raw.Index != a.AuthIndex || json.Unmarshal(raw.JSON, &cred) != nil || cred.Type != "codex" || strings.TrimSpace(cred.Upstream) == "" {
		return credential{}, errors.New("account_binding_changed")
	}
	if cred.Token == "" {
		cred.Token = cred.Fallback
	}
	cred.Fallback = ""
	if r.Status != "" || r.Disabled || r.Unavailable {
		cred.status = status
		cred.statusAt = statusAt
	}
	return cred, nil
}

func bindingID(a store.Account, upstream string) string {
	sum := sha256.Sum256([]byte(a.ID + "\x00" + a.AuthIndex + "\x00" + upstream))
	return hex.EncodeToString(sum[:])
}

// Identity reads the host binding without requiring a usable access token.
// Credentials remain transient; only the binding digest leaves this adapter.
func (c *CPA) Identity(ctx context.Context, a store.Account) (store.Account, error) {
	cred, err := c.readBinding(ctx, a)
	if err != nil {
		return a, err
	}
	a.Binding = bindingID(a, cred.Upstream)
	if cred.status != "" && a.Status != "binding_unavailable" {
		a.Status = cred.status
	}
	return a, nil
}
func (c *CPA) client(cred credential) (*http.Client, error) {
	proxy := ""
	for _, p := range []string{cred.Proxy, cred.ProxyCamel, cred.ProxyDash, cred.ProxyUpper} {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.EqualFold(p, "none") || strings.EqualFold(p, "direct") {
			p = "direct"
		}
		if proxy != "" && proxy != p {
			return nil, errors.New("quota_proxy_invalid")
		}
		proxy = p
	}
	if proxy == "" {
		proxy = "direct"
	}
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
				u, err := url.Parse(proxy)
				if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
					return nil, errors.New("quota_proxy_invalid")
				}
				switch strings.ToLower(u.Scheme) {
				case "http", "https", "socks5", "socks5h":
				default:
					return nil, errors.New("quota_proxy_invalid")
				}
				t.Proxy = http.ProxyURL(u)
			}
			// Bound transports including credential-bearing proxy keys. Active requests
			// retain their transport; retiring only closes idle connections.
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
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func (c *CPA) Query(ctx context.Context, a store.Account) (json.RawMessage, error) {
	raw, _, err := c.QueryWithStatus(ctx, a)
	return raw, err
}

func (c *CPA) QueryWithStatus(ctx context.Context, a store.Account) (json.RawMessage, *store.Account, error) {
	var observed *store.Account
	raw, err := c.query(ctx, a, &observed)
	return raw, observed, err
}

func (c *CPA) query(ctx context.Context, a store.Account, observed **store.Account) (json.RawMessage, error) {
	if a.Status == "binding_unavailable" || a.AuthIndex == "" {
		return nil, errors.New("account_identity_unavailable")
	}
	cred, err := c.readBinding(ctx, a)
	if err != nil {
		return nil, err
	}
	if a.Binding != "" && a.Binding != bindingID(a, cred.Upstream) {
		return nil, errors.New("account_binding_changed")
	}
	observe := func(credential credential) {
		if credential.status != "" {
			*observed = &store.Account{ID: a.ID, AuthIndex: a.AuthIndex, Binding: bindingID(a, credential.Upstream), Status: credential.status, StatusObservedAt: credential.statusAt}
		}
	}
	observe(cred)
	if !validQuotaToken(cred.Token) {
		return nil, errors.New("quota_credential_missing")
	}
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = "https://chatgpt.com/backend-api/wham/usage"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("quota_request_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	req.Header.Set("Chatgpt-Account-Id", cred.Upstream)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-window-refresh-v2")
	client, err := c.client(cred)
	if err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("quota_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		after, bindingErr := c.readBinding(ctx, a)
		if bindingErr != nil || after.Upstream != cred.Upstream {
			*observed = nil
			return nil, errors.New("account_binding_changed")
		}
		observe(after)
		switch resp.StatusCode {
		case 401:
			return nil, errors.New("quota_authentication_failed")
		case 403:
			return nil, errors.New("quota_upstream_forbidden")
		case 429:
			return nil, errors.New("quota_upstream_rate_limited")
		}
		return nil, errors.New("quota_upstream_http_failed")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	defer clear(raw)
	if err != nil {
		return nil, errors.New("quota_body_failed")
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("quota_response_too_large")
	}
	// Revalidate after network work, including the actual upstream account ID.
	after, err := c.binding(ctx, a)
	if err != nil || after.Upstream != cred.Upstream {
		*observed = nil
		return nil, errors.New("account_binding_changed")
	}
	observe(after)
	at := time.Now().UTC()
	if at.Before(started) {
		return nil, errors.New("quota_clock_changed")
	}
	observation, err := quota.Normalize(raw, nil, bindingID(a, cred.Upstream), "cpa_wham_usage", at, started)
	if err != nil {
		return nil, errors.New("quota_response_unrecognized")
	}
	// Only normalized, allowlisted fields reach durable storage.
	return json.Marshal(observation)
}
func (c *CPA) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.transports {
		t.CloseIdleConnections()
	}
	c.transports = nil
}
