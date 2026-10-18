package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

func historyService(t *testing.T) (*Service, *sql.DB, *wakeSource) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	storage, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	source := &wakeSource{}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "backend.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return service, db, source
}

func requestHistory(t *testing.T, service *Service, query string) store.TaskHistoryPage {
	t.Helper()
	response := request(service, "GET", "/api/v2/task-history?"+query, "", "")
	var page store.TaskHistoryPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Records == nil || page.InstanceID != service.instance {
		t.Fatal(response.Code, response.Body.String())
	}
	return page
}

func TestTaskHistoryHTTPPaginationFiltersCountsAndNoSideEffects(t *testing.T) {
	service, db, source := historyService(t)
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 27; index++ {
		id, trigger := fmt.Sprintf("history-query-%02d", index), "manual_single"
		phase := []string{"queued", "completed", "failed"}[index%3]
		if index%2 == 0 {
			trigger = "wake_verification"
		}
		stamp := start.Add(time.Duration(index) * time.Second).Format("2006-01-02T15:04:05.000000000Z")
		if _, err := db.Exec(`INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES(?,?,?,'Needle-account',?,?,?)`, id, id, id, phase, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO query_plans VALUES(?,?,0,0,0)", id, trigger); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"unknown", "completed", "failed", "skipped", "cancelled"} {
		id := "history-wake-" + phase
		stamp := start.Format("2006-01-02T15:04:05.000000000Z")
		if _, err := db.Exec(`INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at,prompt) VALUES(?,?,'private-auth-index','private-binding','daily','cycle',?,?,?,?,'private-prompt')`, id, id, phase, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var versionBefore, versionAfter int
	if err := db.QueryRow("PRAGMA data_version").Scan(&versionBefore); err != nil {
		t.Fatal(err)
	}
	before := requestHistory(t, service, "limit=100")
	seen := map[string]bool{}
	for pageNumber := 1; pageNumber <= 4; pageNumber++ {
		page := requestHistory(t, service, fmt.Sprintf("page=%d&limit=10", pageNumber))
		if page.Total != 32 || page.StatusCounts != (store.TaskHistoryStatusCounts{All: 32, Queued: 9, Completed: 10, Failed: 10, Unknown: 1, Skipped: 1, Cancelled: 1}) || page.Page != pageNumber || page.Limit != 10 || page.Revision != before.Revision {
			t.Fatalf("history page: %+v", page)
		}
		for _, record := range page.Records {
			if seen[record.JobID] {
				t.Fatal("duplicate record across pages", record.JobID)
			}
			seen[record.JobID] = true
			if record.Status == "unknown" && record.Querying != 0 {
				t.Fatal("unknown shown as running", record)
			}
		}
	}
	if len(seen) != 32 {
		t.Fatal("history truncated at recent 20", len(seen))
	}
	filters := url.Values{
		"search": {"NEEDLE"}, "type": {"query"}, "source": {"automatic"}, "trigger": {"wake_verification"}, "status": {"failed"},
		"from": {start.Add(2 * time.Second).Format(time.RFC3339)}, "to": {start.Add(26 * time.Second).Format(time.RFC3339)},
		"today_from": {start.Add(8 * time.Second).Format(time.RFC3339)}, "today_to": {start.Add(21 * time.Second).Format(time.RFC3339)},
	}
	filtered := requestHistory(t, service, filters.Encode())
	if filtered.Total != 4 || len(filtered.Records) != 4 || filtered.StatusCounts.All != 12 || filtered.StatusCounts.Failed != 4 || filtered.StatusCounts.Completed != 4 || filtered.StatusCounts.Queued != 4 || filtered.Summary.Today != 7 || filtered.Summary.TodayAutomatic != 7 || filtered.Summary.TodayManual != 0 {
		t.Fatalf("combined bindings and boundaries: %+v", filtered)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if response := request(service, method, "/api/v2/task-history", `{"account_id":"new-task"}`, "history-write"); response.Code != 404 {
			t.Fatal("history write accepted", method, response.Code, response.Body.String())
		}
	}
	for _, secret := range []string{"private-prompt", "private-binding", "private-auth-index"} {
		if page := requestHistory(t, service, "search="+url.QueryEscape(secret)); page.Total != 0 {
			t.Fatal("private field searchable", secret)
		}
	}
	after := requestHistory(t, service, "limit=100")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("history GET changed persisted records/revision")
	}
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-prompt", "private-binding", "private-auth-index", `"prompt"`, `"binding"`, `"auth_index"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("privacy leak", secret)
		}
	}
	if err = db.QueryRow("PRAGMA data_version").Scan(&versionAfter); err != nil || versionBefore != versionAfter {
		t.Fatal("history API committed database writes", versionBefore, versionAfter, err)
	}
	if len(service.wake) != 0 || len(service.wakeWork) != 0 || len(service.generationWake) != 0 {
		t.Fatal("history API scheduled work")
	}
	assertNoWakeReadsOrSends(t, source)
}

func TestTaskHistoryHTTPRejectsInvalidAndRepeatedParameters(t *testing.T) {
	service, _, source := historyService(t)
	invalid := []string{
		"unknown=1", "page=0", "page=-1", "page=1.5", "page=9999999999999999999999999", "limit=0", "limit=101", "limit=no",
		"type=generation", "source=internal", "status=sending", "trigger=unknown", "search=%00", "search=%FF", "search=%ZZ", "search=x;type=query",
		"search=" + strings.Repeat("x", 257), "search=" + strings.Repeat("x", 4097),
		"from=garbage", "to=2026-10-19T00:00:00", "from=2026-10-20T00:00:00Z&to=2026-10-19T00:00:00Z",
		"today_from=2026-10-19T00:00:00Z", "today_to=2026-10-20T00:00:00Z", "today_from=bad&today_to=worse",
	}
	for _, name := range []string{"page", "limit", "search", "type", "source", "trigger", "status", "from", "to", "today_from", "today_to"} {
		invalid = append(invalid, name+"=1&"+name+"=1")
	}
	invalid = append(invalid, "type=query&%74ype=query")
	for _, query := range invalid {
		response := request(service, "GET", "/api/v2/task-history?"+query, "", "")
		var body map[string]any
		if response.Code != 400 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["error"] != "invalid_task_history_filter" {
			t.Fatalf("invalid query accepted: %q status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	if page := requestHistory(t, service, ""); page.Total != 0 || page.Page != 1 || page.Limit != 10 || page.StatusCounts.All != 0 {
		t.Fatalf("empty history contract: %+v", page)
	}
	assertNoWakeReadsOrSends(t, source)
}

func TestTaskHistoryHTTPStorageFailureIsSanitized(t *testing.T) {
	service, _, source := historyService(t)
	if err := service.Store.Close(); err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/task-history", "", "")
	var body map[string]any
	if response.Code != 503 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["error"] != "storage_unavailable" || strings.Contains(response.Body.String(), "database") {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := service.Store.TaskHistory(context.Background(), store.TaskHistoryFilter{}); err == nil {
		t.Fatal("closed store did not fail")
	}
	assertNoWakeReadsOrSends(t, source)
}

func TestTaskHistoryHTTPTimesSurviveJobDetailMerge(t *testing.T) {
	service, db, source := historyService(t)
	ctx := context.Background()
	account := store.Account{ID: "timed-account", AuthIndex: "private-index", Status: "available"}
	id, _, err := service.Store.Create(ctx, "timed-query", "timed-hash", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Store.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Store.Expand(ctx, job, []store.Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := service.Store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Store.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err = db.Exec(`INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at) VALUES('unstarted-wake','wake-account','private-index','private-binding','manual','cycle','completed',?,?,?)`, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/task-history", "", "")
	var page struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Records) != 2 {
		t.Fatal(response.Code, response.Body.String())
	}
	var record map[string]json.RawMessage
	for _, item := range page.Records {
		if item["started_at"] == nil || item["finished_at"] == nil {
			t.Fatal("history time keys are not stable", item)
		}
		if string(item["type"]) == `"query"` {
			record = item
		} else if string(item["started_at"]) != `""` || string(item["finished_at"]) != `""` {
			t.Fatal("wake timing invented without evidence", item)
		}
	}
	if record == nil {
		t.Fatal("query record missing")
	}
	started, finished := string(record["started_at"]), string(record["finished_at"])
	if started == `""` || finished == `""` || finished != string(record["updated_at"]) {
		t.Fatal("query duration evidence missing", record)
	}
	detail := request(service, "GET", "/api/v2/jobs?job_id="+url.QueryEscape(id), "", "")
	var fields map[string]json.RawMessage
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &fields) != nil {
		t.Fatal(detail.Code, detail.Body.String())
	}
	for _, name := range []string{"started_at", "finished_at"} {
		if _, exists := fields[name]; exists {
			t.Fatal("detail must not overwrite list evidence with empty fields", name, fields)
		}
	}
	for name, value := range fields {
		record[name] = value
	}
	if string(record["started_at"]) != started || string(record["finished_at"]) != finished {
		t.Fatal("detail merge discarded query duration", record)
	}
	after := request(service, "GET", "/api/v2/task-history", "", "")
	if after.Code != 200 || after.Body.String() != response.Body.String() {
		t.Fatal("detail read changed list timing/revision", after.Code, after.Body.String())
	}
	assertNoWakeReadsOrSends(t, source)
}
