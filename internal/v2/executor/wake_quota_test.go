package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestWakeReadsBoundQuotaHeadersAndStructuredEventsWithoutTrustingReplyText(t *testing.T) {
	for _, mode := range []string{"headers", "event", "text", "malformed", "tokens"} {
		t.Run(mode, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			quotaEvent := `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":1,"window_minutes":300,"reset_after_seconds":17900}}}`
			switch mode {
			case "headers":
				host.opened.Headers = http.Header{"X-Codex-Primary-Used-Percent": {"1"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"17900"}, "Authorization": {"Bearer never-persist"}}
			case "event":
				host.chunks[0].Payload = append([]byte("data: "+quotaEvent+"\n\n"), host.chunks[0].Payload...)
			case "text":
				host.chunks[0].Payload = generationCompleted(quotaEvent)
			case "malformed":
				host.opened.Headers = http.Header{"X-Codex-Primary-Used-Percent": {"-1"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"17900"}}
			case "tokens":
				host.chunks[0].Payload = []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`)
			}
			response, err := cpa.ExecuteWakeWithResponse(context.Background(), account, "fixture-model", "hello")
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "headers" || mode == "event"
			if (response.Quota != nil) != want {
				t.Fatal(mode, response.Quota)
			}
			if want && (response.Quota.Binding != account.Binding || response.Quota.FiveHour.State != core.WindowActive || response.Quota.FiveHour.Remaining.Value != 99) {
				t.Fatal(response.Quota)
			}
			if strings.Contains(response.ResponseText, "never-persist") {
				t.Fatal("raw headers leaked")
			}
		})
	}
}
