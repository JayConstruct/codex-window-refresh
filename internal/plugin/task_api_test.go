package plugin

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"codex-window-refresh/internal/core"
	scheduler "codex-window-refresh/internal/runtime"
)

type taskAPIEngine struct {
	fakeEngine
	filter  scheduler.HistoryFilter
	request core.PromptGenerationRequest
	calls   int
}

func (engine *taskAPIEngine) History(filter scheduler.HistoryFilter) scheduler.HistoryPage {
	engine.filter = filter
	return scheduler.HistoryPage{Entries: []core.HistoryEntry{{Kind: "query", AccountID: filter.AccountID, Status: "completed"}}, RetentionDays: 30, MaximumEntries: 10000}
}
func (engine *taskAPIEngine) QueuePromptGeneration(request core.PromptGenerationRequest) string {
	engine.calls++
	engine.request = request
	return "queued"
}

func TestHistoryFiltersAreAuthenticatedBoundedAndStrict(t *testing.T) {
	engine := &taskAPIEngine{}
	for _, values := range []url.Values{{"limit": {"101"}}, {"cursor": {"-1"}}, {"kind": {"other"}}, {"status": {"unsafe"}}, {"from": {"not-time"}}, {"account_id": {"a", "b"}}, {"unknown": {"value"}}, {"from": {"2026-10-15T00:00:00Z"}, "until": {"2026-10-14T00:00:00Z"}}} {
		if result := historyResponse(engine, values); result.StatusCode != 400 {
			t.Fatalf("invalid history filter accepted: %v", values)
		}
	}
	result := historyResponse(engine, url.Values{"account_id": {"<img>"}, "kind": {"query"}, "cursor": {"12"}, "limit": {"2"}})
	if result.StatusCode != 200 || engine.filter.AccountID != "<img>" || engine.filter.Cursor != 12 || engine.filter.Limit != 2 {
		t.Fatal("history filter lost exact values")
	}
	service, _, _, _ := setupService(t)
	service.mu.Lock()
	service.engine = engine
	service.mu.Unlock()
	if result := managementCall(t, service, "GET", ResourcePrefix+"/history"); result.StatusCode != 404 {
		t.Fatal("history exposed as public asset")
	}
	if result := managementCall(t, service, "GET", ManagementPrefix+"/history?kind=query&limit=2"); result.StatusCode != 200 {
		t.Fatal("history query string not routed")
	}
}

func TestGenerationAPIRejectsDuplicateUnknownNullAndTrailingFields(t *testing.T) {
	engine := &taskAPIEngine{}
	done := make(chan struct{})
	for _, body := range []string{`{"account_id":"a","account_id":"b"}`, `{"model":null}`, `{"unknown":"value"}`, `{} {}`, `[]`} {
		if result := generationResponse(engine, WrapperConfig{}, done, false, []byte(body)); result.StatusCode != 400 {
			t.Fatalf("invalid generation body accepted: %s", body)
		}
	}
	if engine.calls != 0 {
		t.Fatal("invalid parameters queued generation")
	}
	request := core.PromptGenerationRequest{RequestID: "request-123", AccountID: "a", Model: "model", Count: 20, Target: "default"}
	body, _ := json.Marshal(request)
	if result := generationResponse(engine, WrapperConfig{}, done, false, body); result.StatusCode != 202 || engine.request != request {
		t.Fatal("generation request not passed intact")
	}
	close(done)
	if result := generationResponse(engine, WrapperConfig{}, done, false, body); result.StatusCode != 409 {
		t.Fatal("stopped scheduler queued model request")
	}
	service, _, _, _ := setupService(t)
	if result := managementCall(t, service, "GET", ManagementPrefix+"/prompt-generation"); result.StatusCode != 404 {
		t.Fatal("GET can invoke generation")
	}
	_ = service.Handle(context.Background(), "management.register", nil)
}
