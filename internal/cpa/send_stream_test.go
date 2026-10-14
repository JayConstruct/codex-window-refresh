package cpa

import (
	"context"
	"encoding/json"
	"testing"

	"codex-window-refresh/internal/core"
)

// These literal JSON responses use the official callback's []byte/base64 wire
// shape. Native Codex ExecuteStream scans SSE lines and emits each scanner line
// as its own callback payload, without adding newlines or blank-frame delimiters.
func TestNativeCodexCallbackLinesPreserveCompletionQuotaAndText(t *testing.T) {
	replies := []string{
		`{"payload":"ZXZlbnQ6IHJlc3BvbnNlLmNyZWF0ZWQ="}`,                                                             // event: response.created
		`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlZCIsInJlc3BvbnNlIjp7InN0YXR1cyI6ImluX3Byb2dyZXNzIn19"}`, // data: {"type":"response.created","response":{"status":"in_progress"}}
		`{"payload":""}`, //
		`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2Uub3V0cHV0X3RleHQuZGVsdGEiLCJkZWx0YSI6Ik9LLiJ9"}`, // data: {"type":"response.output_text.delta","delta":"OK."}
		`{"payload":"ZGF0YTogeyJ0eXBlIjoiY29kZXgucmF0ZV9saW1pdHMiLCJyYXRlX2xpbWl0cyI6eyJhbGxvd2VkIjp0cnVlLCJwcmltYXJ5Ijp7InVzZWRfcGVyY2VudCI6NSwid2luZG93X21pbnV0ZXMiOjMwMCwicmVzZXRfYWZ0ZXJfc2Vjb25kcyI6MzUwMH0sInNlY29uZGFyeSI6eyJ1c2VkX3BlcmNlbnQiOjEyLCJ3aW5kb3dfbWludXRlcyI6MTAwODAsInJlc2V0X2FmdGVyX3NlY29uZHMiOjg2MDAwfX19"}`, // data: {"type":"codex.rate_limits","rate_limits":{"allowed":true,"primary":{"used_percent":5,"window_minutes":300,"reset_after_seconds":3500},"secondary":{"used_percent":12,"window_minutes":10080,"reset_after_seconds":86000}}}
		`{"payload":"ZXZlbnQ6IHJlc3BvbnNlLmNvbXBsZXRlZA=="}`, // event: response.completed
		`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY29tcGxldGVkIiwicmVzcG9uc2UiOnsic3RhdHVzIjoiY29tcGxldGVkIiwib3V0cHV0IjpbeyJjb250ZW50IjpbeyJ0eXBlIjoib3V0cHV0X3RleHQiLCJ0ZXh0IjoiT0suIn1dfV19fQ=="}`, // data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"OK."}]}]}}
		`{"done":true}`,
	}
	result, text, sends, closed := readLiteralModelReplies(t, replies)
	if result.Disposition != core.SendCompleted || result.ErrorCode != "" || sends != 1 || !closed {
		t.Fatalf("native line stream failed: %+v, sends=%d closed=%v", result, sends, closed)
	}
	if text.deltas != "OK." || text.completed != "OK." {
		t.Fatalf("native text events were lost: %+v", text)
	}
	if result.Observation == nil || result.Observation.FiveHour == nil || result.Observation.Weekly == nil || result.Observation.FiveHour.State != core.WindowActive || result.Observation.Weekly.State != core.WindowActive || result.Observation.FiveHour.OperationID != "native-op" {
		t.Fatalf("native quota events were lost: %+v", result.Observation)
	}
}

func TestNativeCodexLineErrorsNeverBecomeSuccessfulOrRetry(t *testing.T) {
	cases := []struct {
		name    string
		replies []string
		code    string
	}{
		{"error frame", []string{
			`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlZCIsInJlc3BvbnNlIjp7InN0YXR1cyI6ImluX3Byb2dyZXNzIn19"}`,
			`{"payload":"ZGF0YTogeyJ0eXBlIjoiZXJyb3IiLCJtZXNzYWdlIjoicmVqZWN0ZWQifQ=="}`,
			`{"done":true}`,
		}, "model_stream_failed"},
		{"error after completion", []string{
			`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY29tcGxldGVkIiwicmVzcG9uc2UiOnsic3RhdHVzIjoiY29tcGxldGVkIn19"}`,
			`{"payload":"ZGF0YTogeyJ0eXBlIjoiZXJyb3IiLCJtZXNzYWdlIjoicmVqZWN0ZWQifQ=="}`,
			`{"done":true}`,
		}, "model_stream_failed"},
		{"callback terminal error", []string{
			`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY29tcGxldGVkIiwicmVzcG9uc2UiOnsic3RhdHVzIjoiY29tcGxldGVkIn19"}`,
			`{"error":"upstream rejected","done":true}`,
		}, "model_stream_failed"},
		{"EOF without completion", []string{`{"payload":"ZGF0YTogeyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlZCIsInJlc3BvbnNlIjp7InN0YXR1cyI6ImluX3Byb2dyZXNzIn19"}`, `{"done":true}`}, "model_stream_missing_completion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, sends, closed := readLiteralModelReplies(t, tc.replies)
			if result.Disposition != core.SendUnknown || result.ErrorCode != tc.code || sends != 1 || !closed {
				t.Fatalf("unsafe native result: %+v, sends=%d closed=%v", result, sends, closed)
			}
		})
	}
}

func TestModelStreamPreservesFragmentedAndMultilineSSE(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		{"split JSON", []string{`data: {"type":"response.comp`, `leted","response":{"status":"completed"}}`, "\n\n"}},
		{"multiline data", []string{"event: response.completed\ndata: {\n", `data: "type": "response.completed",`, "\ndata: \"response\": {\"status\":\"completed\"}}\n\n"}},
		{"native multiline data", []string{"event: response.completed", "data: {", `data: "type": "response.completed",`, `data: "response": {"status":"completed"}}`}},
		{"fragmented string starting with data field text", []string{`data: {"type":"response.completed","response":{"status":"completed","id":"`, `data:escaped\\\"value"}}`}},
		{"split CRLF separators", []string{"event: response.completed\r", "\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\r", "\n\r", "\n"}},
		{"fragmented raw JSON", []string{`{"type":"response.comp`, `leted","response":{"status":"completed"}}`}},
		{"native metadata", []string{": keepalive", "id: 42", "retry: 1000", "event: response.completed", `data: {"type":"response.completed","response":{"status":"completed"}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var replies []string
			for _, chunk := range tc.chunks {
				raw, err := json.Marshal(map[string]any{"payload": []byte(chunk)})
				if err != nil {
					t.Fatal(err)
				}
				replies = append(replies, string(raw))
			}
			replies = append(replies, `{"done":true}`)
			result, _, sends, closed := readLiteralModelReplies(t, replies)
			if result.Disposition != core.SendCompleted || sends != 1 || !closed {
				t.Fatalf("stream framing failed: %+v sends=%d closed=%v", result, sends, closed)
			}
		})
	}
}

func readLiteralModelReplies(t *testing.T, replies []string) (core.SendResult, *generationText, int, bool) {
	t.Helper()
	auth := &fakeCallbacks{upstream: "real"}
	index, sends := 0, 0
	closed := false
	callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
		switch method {
		case "host.model.execute_stream":
			sends++
			return json.Unmarshal([]byte(`{"status_code":200,"headers":{},"stream_id":"native-stream"}`), response)
		case "host.model.stream_read":
			if index >= len(replies) {
				t.Fatal("read past literal terminal reply")
			}
			raw := replies[index]
			index++
			return json.Unmarshal([]byte(raw), response)
		case "host.model.stream_close":
			closed = true
			return json.Unmarshal([]byte(`{}`), response)
		default:
			return auth.Call(ctx, method, request, response)
		}
	})
	host := NewHost(callback, nil)
	accounts, err := host.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := &generationText{}
	result, err := host.sendExact(context.Background(), core.ExactRequest{AccountID: accounts[0].ID, Generation: accounts[0].Generation, OperationID: "native-op", Model: "model", Prompt: "Reply OK."}, text)
	if err != nil {
		t.Fatal(err)
	}
	return result, text, sends, closed
}
