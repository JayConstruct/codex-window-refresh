package cpa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var ErrBridgeRead = errors.New("quota_management_bridge_failed")

// BridgeReader explicitly delegates token substitution to CPA's authenticated
// management api-call. Its response/error is never logged or attached to errors.
type BridgeReader struct {
	ManagementURL    string
	ManagementKeyEnv string
	Getenv           func(string) string
	Client           *http.Client
}

func (b *BridgeReader) Read(ctx context.Context, a AuthBinding) ([]byte, error) {
	if b == nil || b.ManagementURL == "" || b.ManagementKeyEnv == "" || a.AuthIndex == "" || a.UpstreamAccountID == "" {
		return nil, ErrMissingBridge
	}
	lookup := b.Getenv
	if lookup == nil {
		lookup = os.Getenv
	}
	key := lookup(b.ManagementKeyEnv)
	if key == "" {
		return nil, ErrMissingBridge
	}
	endpoint, err := url.Parse(b.ManagementURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, ErrMissingBridge
	}
	switch strings.TrimSuffix(endpoint.Path, "/") {
	case "", "/":
		endpoint.Path = "/v0/management/api-call"
	case "/v0/management":
		endpoint.Path = "/v0/management/api-call"
	case "/v0/management/api-call", "/v8/management/requests/api-call":
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/")
	default:
		return nil, ErrMissingBridge
	}
	request := struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
	}{a.AuthIndex, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": a.UpstreamAccountID, "Accept": "application/json"}}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, ErrBridgeRead
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrBridgeRead
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	if b.Client != nil {
		*client = *b.Client
		if client.Timeout == 0 {
			client.Timeout = 60 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrBridgeRead
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrBridgeRead
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return nil, ErrBridgeRead
	}
	var result struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if json.Unmarshal(data, &result) != nil || result.StatusCode != http.StatusOK || !json.Valid([]byte(result.Body)) {
		return nil, ErrBridgeRead
	}
	return []byte(result.Body), nil
}

// Available checks explicit bridge configuration without contacting CPA.
func (b *BridgeReader) Available() bool {
	return b.ConfigurationStatus() == "configured"
}

// ConfigurationStatus is a local check, not a network or authentication probe.
// It returns fixed codes and never includes the key or configured URL.
func (b *BridgeReader) ConfigurationStatus() string {
	if b == nil || b.ManagementURL == "" {
		return "missing_management_url"
	}
	if b.ManagementKeyEnv == "" {
		return "missing_management_key_env"
	}
	lookup := b.Getenv
	if lookup == nil {
		lookup = os.Getenv
	}
	if lookup(b.ManagementKeyEnv) == "" {
		return "missing_management_key"
	}
	endpoint, err := url.Parse(b.ManagementURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "invalid_management_url"
	}
	switch strings.TrimSuffix(endpoint.Path, "/") {
	case "", "/", "/v0/management", "/v0/management/api-call", "/v8/management/requests/api-call":
		return "configured"
	}
	return "invalid_management_url"
}
