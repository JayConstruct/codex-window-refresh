package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

const resetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const resetConsumeURL = resetCreditsURL + "/consume"

// ResetTransport reports local configuration without probing account services.
// CLI is an explicit compatibility option, never an automatic mutation retry.
func (c *CPA) ResetTransport() (string, error) {
	if c.CodexBinary != "" {
		_, err := c.ResetCLI()
		return "codex_cli", err
	}
	if c.Host == nil {
		return "cpa_http", errors.New("bridge_unavailable")
	}
	return "cpa_http", nil
}

func (c *CPA) resetHTTP(ctx context.Context, a store.Account, method string, body []byte) (quotaHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cred, err := c.resetBinding(ctx, a)
	if err != nil {
		return quotaHTTPResponse{}, err
	}
	if !validQuotaToken(cred.Token) || a.Binding == "" || a.Binding != bindingID(a, cred.Upstream) {
		return quotaHTTPResponse{}, errors.New("account_binding_changed")
	}
	proxy, err := accountQuotaProxy(cred)
	if err != nil {
		return quotaHTTPResponse{}, err
	}
	if proxy == "" && c.ResetProxy != "" {
		proxy, err = accountQuotaProxy(credential{Proxy: c.ResetProxy})
		if err != nil {
			return quotaHTTPResponse{}, err
		}
	}
	endpoint := resetCreditsURL
	if method == http.MethodPost {
		endpoint = resetConsumeURL
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+cred.Token)
	headers.Set("Chatgpt-Account-Id", cred.Upstream)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "codex-window-refresh-v2")
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Originator", "Codex Desktop")
	if method == http.MethodPost {
		headers.Set("Content-Type", "application/json")
	}
	var response quotaHTTPResponse
	if proxy == "" && c.transport == nil {
		response, err = c.hostAccountHTTP(ctx, method, endpoint, headers, body)
	} else {
		if proxy == "" {
			proxy = "direct"
		}
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err == nil {
			req.Header = headers
			var resp *http.Response
			resp, err = c.quotaClient(proxy).Do(req)
			if err == nil {
				response.StatusCode = resp.StatusCode
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					response.Body, err = io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
				}
				_ = resp.Body.Close()
			}
		}
	}
	if err != nil {
		clear(response.Body)
		if ctx.Err() != nil {
			return quotaHTTPResponse{}, ctx.Err()
		}
		return quotaHTTPResponse{}, errors.New("reset_transport_failed")
	}
	if len(response.Body) > maxQuotaBody {
		clear(response.Body)
		return quotaHTTPResponse{}, errors.New("reset_response_too_large")
	}
	// The account must still refer to the credential that performed this read or
	// redemption. A post-send failure stays unknown and reuses the persisted key.
	after, err := c.resetBinding(ctx, a)
	if err != nil || bindingID(a, after.Upstream) != a.Binding {
		clear(response.Body)
		return quotaHTTPResponse{}, errors.New("account_binding_changed")
	}
	code := ""
	switch {
	case response.StatusCode == 401:
		code = "reset_authentication_failed"
	case response.StatusCode == 403:
		code = "reset_permission_denied"
	case response.StatusCode == 429:
		code = "reset_rate_limited"
	case response.StatusCode < 200 || response.StatusCode >= 300:
		code = "reset_upstream_failed"
	}
	if code != "" {
		clear(response.Body)
		return quotaHTTPResponse{}, errors.New(code)
	}
	return response, nil
}

func (c *CPA) readResetCreditsHTTP(ctx context.Context, a store.Account) (*policy.ResetCredits, error) {
	response, err := c.resetHTTP(ctx, a, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	defer clear(response.Body)
	var payload struct {
		Count      *int              `json:"available_count"`
		CamelCount *int              `json:"availableCount"`
		Credits    []json.RawMessage `json:"credits"`
	}
	if json.Unmarshal(response.Body, &payload) != nil || len(payload.Credits) > 1000 {
		return nil, errors.New("reset_protocol_invalid")
	}
	count := payload.Count
	if count == nil {
		count = payload.CamelCount
	}
	if count != nil && (*count < 0 || *count > 100000 || payload.Count != nil && payload.CamelCount != nil && *payload.Count != *payload.CamelCount) {
		return nil, errors.New("reset_protocol_invalid")
	}
	if count == nil && payload.Credits == nil {
		return nil, errors.New("reset_protocol_invalid")
	}
	result := &policy.ResetCredits{}
	if payload.Credits != nil {
		result.Credits = []policy.ResetCredit{}
	}
	available := 0
	for _, raw := range payload.Credits {
		var card struct {
			ID           string          `json:"id"`
			Type         string          `json:"reset_type"`
			CamelType    string          `json:"resetType"`
			Status       string          `json:"status"`
			Expires      json.RawMessage `json:"expires_at"`
			CamelExpires json.RawMessage `json:"expiresAt"`
		}
		if json.Unmarshal(raw, &card) != nil {
			return nil, errors.New("reset_protocol_invalid")
		}
		if card.Type == "" {
			card.Type = card.CamelType
		} else if card.CamelType != "" && card.Type != card.CamelType {
			return nil, errors.New("reset_protocol_invalid")
		}
		if card.Type == "codex_rate_limits" {
			card.Type = "codexRateLimits"
		}
		if card.Status == "available" && card.Type == "codexRateLimits" {
			available++
		}
		if !policy.RoutingLabel(card.ID) {
			return nil, errors.New("reset_protocol_invalid")
		}
		if !policy.RoutingLabel(card.Type) || !policy.RoutingLabel(card.Status) {
			return nil, errors.New("reset_protocol_invalid")
		}
		expires := card.Expires
		if len(expires) == 0 {
			expires = card.CamelExpires
		}
		var expiry *int64
		var text string
		if json.Unmarshal(expires, &text) == nil {
			if at, err := time.Parse(time.RFC3339Nano, text); err == nil && at.Unix() > 0 {
				v := at.Unix()
				expiry = &v
			}
		} else {
			var stamp int64
			if json.Unmarshal(expires, &stamp) == nil && stamp > 0 {
				expiry = &stamp
			}
		}
		result.Credits = append(result.Credits, policy.ResetCredit{ID: card.ID, ResetType: card.Type, Status: card.Status, ExpiresAt: expiry})
	}
	result.AvailableCount = available
	if count != nil {
		result.AvailableCount = *count
	}
	return result, nil
}

func (c *CPA) consumeResetCreditHTTP(ctx context.Context, a store.Account, creditID, key string) (string, error) {
	body, _ := json.Marshal(map[string]string{"credit_id": creditID, "redeem_request_id": key})
	defer clear(body)
	response, err := c.resetHTTP(ctx, a, http.MethodPost, body)
	if err != nil {
		return "", err
	}
	defer clear(response.Body)
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(response.Body, &payload) != nil {
		return "", errors.New("reset_protocol_invalid")
	}
	switch payload.Code {
	case "reset":
		return "reset", nil
	case "already_redeemed":
		return "alreadyRedeemed", nil
	case "nothing_to_reset":
		return "nothingToReset", nil
	case "no_credit":
		return "noCredit", nil
	}
	return "", errors.New("reset_protocol_invalid")
}
