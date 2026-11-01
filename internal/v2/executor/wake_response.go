package executor

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/quota"
	"codex-window-refresh/internal/v2/store"
)

type wakeRecorder struct {
	response *store.WakeResponse
	prompt   string
	text     string
	binding  string
	started  time.Time
}

func (recorder *wakeRecorder) quota(raw []byte, headers http.Header, source string) {
	if recorder.binding == "" || recorder.started.IsZero() {
		return
	}
	// Only provider fields are normalized; raw response headers are never saved.
	snapshot, err := quota.Normalize(raw, headers, recorder.binding, source, time.Now().UTC(), recorder.started)
	if err != nil {
		return
	}
	var old []byte
	if recorder.response.Quota != nil {
		old, _ = json.Marshal(recorder.response.Quota)
	}
	incoming, _ := json.Marshal(snapshot)
	merged, changed, err := quota.Merge(old, incoming)
	if err == nil && changed {
		var result quota.Snapshot
		if json.Unmarshal(merged, &result) == nil {
			recorder.response.Quota = &result
		}
	}
}

func (recorder *wakeRecorder) capture(raw json.RawMessage) {
	var event struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if event.Type == "codex.rate_limits" {
		recorder.quota(raw, nil, "cpa_websocket_quota")
		return
	}
	if event.Type != "" {
		recorder.response.Event = event.Type
	}
	if event.Response.Status != "" {
		recorder.response.Status = event.Response.Status
	}
	for _, rawError := range []json.RawMessage{event.Error, event.Response.Error} {
		var failure struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if len(rawError) != 0 && json.Unmarshal(rawError, &failure) == nil {
			recorder.response.UpstreamErrorCode = failure.Code
			recorder.response.UpstreamErrorType = failure.Type
			recorder.response.ErrorMessage = recorder.clean(failure.Message)
		}
	}
	if event.Type == "response.output_text.delta" {
		recorder.append(event.Delta)
	}
	if event.Type == "response.completed" {
		var completed strings.Builder
		for _, output := range event.Response.Output {
			for _, content := range output.Content {
				if content.Type == "output_text" {
					remaining := 4096 - completed.Len()
					if len(content.Text) > remaining {
						recorder.response.Truncated = true
						content.Text = content.Text[:remaining]
					}
					completed.WriteString(content.Text)
				}
			}
		}
		if completed.Len() > 0 {
			recorder.text = completed.String()
		}
	}
}

func (recorder *wakeRecorder) append(text string) {
	remaining := 4096 - len(recorder.text)
	if len(text) > remaining {
		text = text[:remaining]
		recorder.response.Truncated = true
	}
	recorder.text += text
}

func (recorder *wakeRecorder) clean(text string) string {
	if recorder.prompt != "" {
		text = strings.ReplaceAll(text, recorder.prompt, "[提示词已隐藏]")
	}
	return store.SanitizeWakeText(text)
}

func (recorder *wakeRecorder) finish() {
	recorder.response.ResponseText = recorder.clean(recorder.text)
}
