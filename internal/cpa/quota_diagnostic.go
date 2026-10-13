package cpa

import (
	"context"
	"errors"
	"net/http"
	"time"

	"codex-window-refresh/internal/core"
)

var (
	ErrQuotaHostOperation    = errors.New("quota_host_operation_failed")
	ErrQuotaTimeout          = errors.New("quota_query_timeout")
	ErrQuotaCancelled        = errors.New("quota_query_cancelled")
	ErrQuotaForbidden        = errors.New("quota_upstream_forbidden")
	ErrQuotaRateLimited      = errors.New("quota_upstream_rate_limited")
	ErrQuotaUpstreamServer   = errors.New("quota_upstream_server_failed")
	ErrQuotaUpstreamHTTP     = errors.New("quota_upstream_http_failed")
	ErrQuotaResponseInvalid  = errors.New("quota_response_invalid")
	ErrQuotaResponseTooLarge = errors.New("quota_response_too_large")
	ErrBridgeAuthentication  = errors.New("quota_management_auth_failed")
	ErrBridgeEndpoint        = errors.New("quota_management_endpoint_missing")
	ErrBridgeHTTP            = errors.New("quota_management_http_failed")
)

type quotaQueryError struct {
	cause      error
	diagnostic core.QuotaDiagnostic
}

func (e *quotaQueryError) Error() string                         { return e.cause.Error() }
func (e *quotaQueryError) Unwrap() error                         { return e.cause }
func (e *quotaQueryError) QuotaDiagnostic() core.QuotaDiagnostic { return e.diagnostic }

func quotaError(cause error, stage string, status int) error {
	return &quotaQueryError{cause: cause, diagnostic: core.QuotaDiagnostic{Code: cause.Error(), Stage: stage, HTTPStatus: status}}
}

func measuredQuotaError(err error, start time.Time, stage string) error {
	if err == nil {
		return nil
	}
	var existing *quotaQueryError
	if errors.As(err, &existing) {
		copy := *existing
		copy.diagnostic.ElapsedMS = max(0, time.Since(start).Milliseconds())
		return &copy
	}
	return &quotaQueryError{cause: err, diagnostic: core.QuotaDiagnostic{Code: err.Error(), Stage: stage, ElapsedMS: max(0, time.Since(start).Milliseconds())}}
}

func quotaRequestError(ctx context.Context, err, fallback error, stage string) error {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return quotaError(ErrQuotaTimeout, stage, 0)
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return quotaError(ErrQuotaCancelled, stage, 0)
	default:
		return quotaError(fallback, stage, 0)
	}
}

func quotaUpstreamError(status int) error {
	var cause error
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusUnauthorized:
		cause = ErrQuotaCredentialExpired
	case status == http.StatusForbidden:
		cause = ErrQuotaForbidden
	case status == http.StatusTooManyRequests:
		cause = ErrQuotaRateLimited
	case status >= 500 && status <= 599:
		cause = ErrQuotaUpstreamServer
	default:
		cause = ErrQuotaUpstreamHTTP
	}
	return quotaError(cause, "upstream_http", status)
}

// Use the request identity configured by the official CPA quota panel. This is
// an explicit HTTP header, not a promise that upstream will accept a credential.
const codexQuotaUserAgent = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
const maxQuotaRequestTimeout = 5 * time.Minute

func quotaRequestHeaders(token, account string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}, "Chatgpt-Account-Id": {account}, "Accept": {"application/json"}, "Content-Type": {"application/json"}, "User-Agent": {codexQuotaUserAgent}}
}
