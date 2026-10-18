package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type responseWakeSource struct{ *workerWakeSource }

func (source *responseWakeSource) ExecuteWakeWithResponse(context.Context, store.Account, string, string) (store.WakeResponse, error) {
	source.wakeCalls.Add(1)
	return store.WakeResponse{Stage: "read", HTTPStatus: 403, Event: "response.failed", Status: "failed", UpstreamErrorCode: "model_not_allowed", ErrorMessage: "Model not allowed api_key=private-secret", ResponseText: "Partial response", Bytes: 500, ErrorCode: "wake_delivery_unknown"}, errors.New("wake_delivery_unknown")
}

func TestWakeWorkerPersistsResponseAndReadsNeverResend(t *testing.T) {
	worker := &workerWakeSource{}
	_, storage := prepareWakeWorker(t, worker)
	source := &responseWakeSource{workerWakeSource: worker}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	initial := acceptWake(t, service, "record-response")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	unknown := awaitWake(t, storage, initial.ID, "unknown")
	cancel()
	<-done
	if unknown.Response == nil || unknown.Response.HTTPStatus != 403 || unknown.Response.ErrorMessage == "" {
		t.Fatal(unknown)
	}
	for _, path := range []string{"/api/v2/wake-jobs?job_id=" + initial.ID, "/api/v2/wake-jobs?limit=20"} {
		response := request(service, "GET", path, "", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), "model_not_allowed") || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal(response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"view_revision":`) {
			t.Fatal("wake reads omit the revision needed for overview polling")
		}
		var body any
		if json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatal("invalid response record JSON")
		}
	}
	if _, err = New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.Wake(context.Background(), initial.ID)
	if err != nil || recovered.Response == nil || recovered.Response.HTTPStatus != 403 || source.wakeCalls.Load() != 1 || recovered.Phase != "unknown" {
		t.Fatal(recovered, err, source.wakeCalls.Load())
	}
}
