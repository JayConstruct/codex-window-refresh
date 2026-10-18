package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

type WakeResponse struct {
	RecordedAt        string `json:"recorded_at"`
	Stage             string `json:"stage"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	Event             string `json:"event,omitempty"`
	Status            string `json:"status,omitempty"`
	UpstreamErrorCode string `json:"upstream_error_code,omitempty"`
	UpstreamErrorType string `json:"upstream_error_type,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	ResponseText      string `json:"response_text,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	Bytes             int    `json:"bytes"`
	Truncated         bool   `json:"truncated"`
	Completed         bool   `json:"completed"`
	CloseFailed       bool   `json:"close_failed"`
}

var wakeSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/-]+=*`),
	regexp.MustCompile(`(?i)(authorization|bearer|access[_-]?token|refresh[_-]?token|api[_-]?key|password|secret|cookie)["'\s:=]+[^\s,;"'<>]+`),
	regexp.MustCompile(`(?i)(?:sk-|sess-)[a-z0-9_-]+|eyJ[a-zA-Z0-9_.-]+`),
	regexp.MustCompile(`(?i)(?:https?|socks5h?)://[^\s<>"']+`),
	regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`),
	regexp.MustCompile(`[a-zA-Z0-9_./+=-]{40,}`),
}

func SanitizeWakeText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	for _, pattern := range wakeSecretPatterns {
		text = pattern.ReplaceAllString(text, "[已隐藏]")
	}
	text = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return -1
		}
		return character
	}, text)
	return strings.TrimSpace(text)
}

func NormalizeWakeResponse(response WakeResponse) WakeResponse {
	switch response.Stage {
	case "preflight", "open", "read", "completed", "close", "interrupted":
	default:
		response.Stage = "interrupted"
	}
	if response.HTTPStatus < 100 || response.HTTPStatus > 599 {
		response.HTTPStatus = 0
	}
	if response.Bytes < 0 {
		response.Bytes = 0
	}
	if response.Bytes > 1<<20 {
		response.Bytes = 1 << 20
		response.Truncated = true
	}
	for _, field := range []*string{&response.Event, &response.Status, &response.UpstreamErrorCode, &response.UpstreamErrorType} {
		value := *field
		if len(value) > 80 || strings.ContainsFunc(value, func(character rune) bool {
			return !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '.' || character == '-')
		}) {
			*field = ""
		} else {
			*field = SanitizeWakeText(value)
		}
	}
	for _, field := range []*string{&response.ErrorMessage, &response.ResponseText} {
		*field = SanitizeWakeText(*field)
		if len(*field) > 2048 {
			*field = strings.ToValidUTF8((*field)[:2048], "")
			response.Truncated = true
		}
	}
	switch response.ErrorCode {
	case "", "wake_model_busy", "account_identity_ambiguous", "account_identity_unavailable", "account_binding_changed", "account_not_found", "account_disabled", "wake_account_unavailable", "generation_not_supported", "wake_not_supported", "wake_request_invalid", "wake_request_rejected", "wake_delivery_unknown", "wake_outcome_unknown", "wake_interrupted", "model_busy", "context_deadline_exceeded", "context_canceled":
	default:
		response.ErrorCode = "wake_outcome_unknown"
	}
	return response
}

func loadWakeResponse(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*WakeResponse, error) {
	var raw string
	err := query.QueryRowContext(ctx, "SELECT detail FROM wake_responses WHERE job_id=?", id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var response WakeResponse
	if err = json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, err
	}
	response = NormalizeWakeResponse(response)
	return &response, nil
}
