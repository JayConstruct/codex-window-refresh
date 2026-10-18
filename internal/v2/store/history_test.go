package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seedHistoryQuery(t *testing.T, storage *Store, id, target, phase, trigger string, automatic bool, at time.Time) {
	t.Helper()
	if _, err := storage.db.Exec(`INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,expanded,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`, id, id, id, target, phase, queryStamp(at), queryStamp(at)); err != nil {
		t.Fatal(err)
	}
	if trigger != "" {
		if _, err := storage.db.Exec("INSERT INTO query_plans VALUES(?,?,?,0,0)", id, trigger, automatic); err != nil {
			t.Fatal(err)
		}
	}
}

func seedHistoryWake(t *testing.T, storage *Store, id, phase, reason string, at time.Time) {
	t.Helper()
	if _, err := storage.db.Exec(`INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at,model,prompt) VALUES(?,?, 'private-auth-index','private-binding',?,'cycle',?,?,?,?, 'history-model','private-prompt')`, id, id+"-account", reason, phase, queryStamp(at), queryStamp(at), queryStamp(at)); err != nil {
		t.Fatal(err)
	}
}

func readHistory(t *testing.T, storage *Store, filter TaskHistoryFilter) TaskHistoryPage {
	t.Helper()
	page, err := storage.TaskHistory(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestTaskHistoryPaginationCountsSearchAndBindings(t *testing.T) {
	storage := openTest(t)
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 27; index++ {
		id := fmt.Sprintf("query-%02d", index)
		phase, trigger := "completed", "manual_single"
		if index%2 == 0 {
			phase, trigger = "failed", "wake_preflight"
		}
		seedHistoryQuery(t, storage, id, "Needle-Account", phase, trigger, false, start.Add(time.Duration(index)*time.Second))
	}
	seedHistoryWake(t, storage, "wake-unknown", "unknown", "daily", start.Add(time.Minute))
	seedHistoryWake(t, storage, "wake-running", "verifying", "manual", start.Add(2*time.Minute))
	filter := TaskHistoryFilter{Type: "query", Source: "automatic", Trigger: "wake_preflight", Search: "NEEDLE", Status: "failed", Limit: 5,
		From: start.Format(time.RFC3339), To: start.Add(time.Hour).Format(time.RFC3339), TodayFrom: start.Format(time.RFC3339), TodayTo: start.Add(24 * time.Hour).Format(time.RFC3339)}
	seen := map[string]bool{}
	for pageNumber := 1; pageNumber <= 3; pageNumber++ {
		filter.Page = pageNumber
		page := readHistory(t, storage, filter)
		if page.Page != pageNumber || page.Total != 14 || page.StatusCounts.All != 14 || page.StatusCounts.Failed != 14 || page.Summary.Today != 14 || page.Summary.TodayAutomatic != 14 || page.Summary.TodayManual != 0 {
			t.Fatalf("filtered page: %+v", page)
		}
		for _, record := range page.Records {
			if seen[record.JobID] || record.Source != "automatic" || record.Status != "failed" {
				t.Fatalf("duplicate or incorrect record: %+v", record)
			}
			seen[record.JobID] = true
		}
	}
	if len(seen) != 14 {
		t.Fatal("pagination omitted persisted records", len(seen))
	}
	all := readHistory(t, storage, TaskHistoryFilter{Limit: 100, TodayFrom: filter.TodayFrom, TodayTo: filter.TodayTo})
	if len(all.Records) != 29 || all.Total != 29 || all.StatusCounts != (TaskHistoryStatusCounts{All: 29, Running: 1, Completed: 13, Failed: 14, Unknown: 1}) || all.Summary.TodayAutomatic != 15 || all.Summary.TodayManual != 14 {
		t.Fatalf("complete counts: %+v", all)
	}
	filter = TaskHistoryFilter{Type: "query", Status: "completed", Limit: 10, Page: 999}
	page := readHistory(t, storage, filter)
	if page.Page != 2 || len(page.Records) != 3 || page.Total != 13 || page.StatusCounts.All != 27 || page.StatusCounts.Failed != 14 {
		t.Fatalf("status-independent counts or clamp: %+v", page)
	}
	for _, search := range []string{"%", "_", "' OR 1=1 --"} {
		if page := readHistory(t, storage, TaskHistoryFilter{Search: search}); page.Total != 0 || page.Records == nil {
			t.Fatalf("search is not literal: %q %+v", search, page)
		}
	}
}

func TestTaskHistoryTimeBoundariesAndRestart(t *testing.T) {
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	start := time.Date(2026, 10, 18, 16, 0, 0, 123456789, time.UTC)
	end := start.Add(24 * time.Hour)
	for index, at := range []time.Time{start.Add(-time.Nanosecond), start, end.Add(-time.Nanosecond), end} {
		seedHistoryQuery(t, storage, fmt.Sprintf("boundary-%d", index), "", "completed", "", false, at)
	}
	filter := TaskHistoryFilter{From: "2026-10-19T00:00:00.123456789+08:00", To: "2026-10-20T00:00:00.123456789+08:00", TodayFrom: start.Format(time.RFC3339Nano), TodayTo: end.Format(time.RFC3339Nano)}
	before := readHistory(t, storage, filter)
	if before.Total != 2 || before.Summary.Today != 2 || before.Summary.TodayManual != 2 || before.Records[0].JobID != "boundary-2" || before.Records[1].JobID != "boundary-1" {
		t.Fatalf("inclusive/exclusive boundaries: %+v", before)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after := readHistory(t, storage, filter)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("restart changed durable history: before=%+v after=%+v", before, after)
	}
}

func TestTaskHistoryProjectionPrivacyAndReadOnly(t *testing.T) {
	storage := openTest(t)
	storage.db.SetMaxOpenConns(1)
	stamp := time.Now().UTC()
	seedHistoryQuery(t, storage, "failed-batch", "", "failed", "manual_batch", false, stamp)
	seedHistoryQuery(t, storage, "queued-query", "queued-account", "running", "wake_verification", false, stamp)
	seedHistoryQuery(t, storage, "automatic-flag", "flag-account", "queued", "manual_single", true, stamp)
	seedHistoryQuery(t, storage, "private-error", "error-account", "failed", "manual_single", false, stamp)
	for _, phase := range []string{"queued", "checking", "prepared", "sending", "verifying", "unknown", "completed", "failed", "skipped", "cancelled"} {
		seedHistoryWake(t, storage, "wake-"+phase, phase, "daily", stamp)
	}
	for _, statement := range []string{
		`INSERT INTO job_items(job_id,account_id,phase,error_code) VALUES('failed-batch','a-item','failed','quota_authentication_failed'),('failed-batch','z-item','failed','quota_transport_failed'),('queued-query','queued-account','queued','')`,
		`UPDATE jobs SET error_code='Authorization: Bearer private-error-token' WHERE id='private-error'`,
		`UPDATE wake_jobs SET error_code='Authorization: Bearer private-error-token',hold_reason='private-binding' WHERE id='wake-unknown'`,
		`UPDATE wake_jobs SET error_code='window_verified' WHERE id='wake-completed'`,
		`UPDATE wake_jobs SET hold_reason='model_missing' WHERE id='wake-queued'`,
		`UPDATE wake_jobs SET hold_reason='configuration_changed' WHERE id='wake-checking'`,
		`PRAGMA query_only=ON`,
	} {
		if _, err := storage.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var changesBefore, changesAfter int
	if err := storage.db.QueryRow("SELECT total_changes()").Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	page := readHistory(t, storage, TaskHistoryFilter{Limit: 100})
	records := map[string]TaskHistoryRecord{}
	for _, record := range page.Records {
		records[record.JobID] = record
	}
	if record := records["failed-batch"]; record.ErrorCode != "quota_authentication_failed" || record.Total != 2 || record.Failed != 2 {
		t.Fatalf("first failed item not projected: %+v", record)
	}
	if record := records["queued-query"]; record.Source != "automatic" || record.Status != "queued" || record.Waiting != 1 || record.Querying != 0 {
		t.Fatalf("internal automatic read misclassified: %+v", record)
	}
	if records["automatic-flag"].Source != "automatic" || records["private-error"].ErrorCode != "quota_query_failed" || records["wake-unknown"].ErrorCode != "wake_outcome_unknown" || records["wake-unknown"].Querying != 0 || records["wake-completed"].ErrorCode != "window_verified" {
		t.Fatalf("classification/sanitization: %+v", records)
	}
	if records["wake-queued"].HoldReason != "model_missing" || records["wake-checking"].HoldReason != "configuration_changed" {
		t.Fatal("valid hold reason lost", records)
	}
	for _, phase := range []string{"completed", "failed", "skipped", "cancelled", "unknown"} {
		if record := records["wake-"+phase]; record.Querying != 0 || record.Waiting != 0 {
			t.Fatalf("terminal/unknown treated as active: %+v", record)
		}
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-auth-index", "private-binding", "private-prompt", "private-error-token", `"auth_index"`, `"binding"`, `"prompt"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private history data exposed", secret)
		}
		if result := readHistory(t, storage, TaskHistoryFilter{Search: secret}); result.Total != 0 {
			t.Fatal("private field searchable", secret)
		}
	}
	if result := readHistory(t, storage, TaskHistoryFilter{Search: "a-item"}); result.Total != 1 || result.Records[0].JobID != "failed-batch" {
		t.Fatal("batch item account search failed", result)
	}
	if result := readHistory(t, storage, TaskHistoryFilter{Search: "quota_authentication_failed"}); result.Total != 1 {
		t.Fatal("item error search failed", result)
	}
	if err = storage.db.QueryRow("SELECT total_changes()").Scan(&changesAfter); err != nil || changesAfter != changesBefore {
		t.Fatal("history caused writes", changesBefore, changesAfter, err)
	}
	if again := readHistory(t, storage, TaskHistoryFilter{Limit: 100}); !reflect.DeepEqual(page, again) {
		t.Fatal("read changed history or revision")
	}
}

func TestTaskHistoryQueryTimesFollowDurableCompletion(t *testing.T) {
	for _, code := range []string{"", "quota_transport_failed"} {
		t.Run("outcome_"+code, func(t *testing.T) {
			storage := openTest(t)
			ctx := context.Background()
			account := Account{ID: "history-account", AuthIndex: "private-index", Status: "available"}
			first := createTest(t, storage, "history-first", account.ID)
			if err := storage.Expand(ctx, first, []Account{account}); err != nil {
				t.Fatal(err)
			}
			run, err := storage.Claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			second := createTest(t, storage, "history-coalesced", account.ID)
			if err = storage.Expand(ctx, second, []Account{account}); err != nil {
				t.Fatal(err)
			}
			before := readHistory(t, storage, TaskHistoryFilter{})
			for _, record := range before.Records {
				if record.StartedAt == "" || record.FinishedAt != "" || record.Status != "running" {
					t.Fatalf("unfinished run has false timing: %+v", record)
				}
			}
			quota := json.RawMessage(`{"permission":"allowed"}`)
			if err = storage.Finish(ctx, run, quota, code); err != nil {
				t.Fatal(err)
			}
			after := readHistory(t, storage, TaskHistoryFilter{})
			for _, record := range after.Records {
				job, jobErr := storage.Job(ctx, record.JobID)
				started, startErr := time.Parse(time.RFC3339Nano, record.StartedAt)
				finished, finishErr := time.Parse(time.RFC3339Nano, record.FinishedAt)
				if jobErr != nil || startErr != nil || finishErr != nil || finished.Before(started) || record.FinishedAt != job.UpdatedAt || record.StartedAt != before.Records[0].StartedAt {
					t.Fatalf("completion does not match committed evidence: record=%+v job=%+v errors=%v/%v/%v", record, job, jobErr, startErr, finishErr)
				}
			}
			if len(after.Records) != 2 || after.Records[0].FinishedAt != after.Records[1].FinishedAt {
				t.Fatalf("coalesced parents differ: %+v", after)
			}
			if err = storage.Finish(ctx, run, quota, code); err != nil {
				t.Fatal(err)
			}
			if replay := readHistory(t, storage, TaskHistoryFilter{}); !reflect.DeepEqual(after, replay) {
				t.Fatalf("result replay changed completion: before=%+v after=%+v", after, replay)
			}
		})
	}
}

func TestTaskHistoryUnstartedTimesAndUnknownPhases(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	stamp := time.Now().UTC()
	for _, phase := range []string{"queued", "blocked", "running", "completed", "failed", "cancelled", "unknown", "future-phase"} {
		seedHistoryQuery(t, storage, "query-"+phase, "account", phase, "manual_single", false, stamp)
		seedHistoryWake(t, storage, "wake-"+phase, phase, "manual", stamp)
	}
	empty := createTest(t, storage, "empty-batch", "")
	if err := storage.Expand(ctx, empty, nil); err != nil {
		t.Fatal(err)
	}
	blocked := createTest(t, storage, "blocked-expiry", "missing-account")
	if _, err := storage.db.Exec("UPDATE jobs SET created_at=? WHERE id=?", queryStamp(stamp.Add(-3*time.Minute)), blocked.ID); err != nil {
		t.Fatal(err)
	}
	if err := storage.Block(ctx, blocked.ID, "bridge_unavailable"); err != nil {
		t.Fatal(err)
	}
	page := readHistory(t, storage, TaskHistoryFilter{Limit: 100})
	for _, record := range page.Records {
		if record.StartedAt != "" {
			t.Fatalf("invented start time: %+v", record)
		}
		wantFinish := record.Type == "query" && (record.Phase == "completed" || record.Phase == "failed" || record.Phase == "cancelled")
		if wantFinish && record.FinishedAt != record.UpdatedAt || !wantFinish && record.FinishedAt != "" {
			t.Fatalf("invented or missing finish time: %+v", record)
		}
		if record.Phase == "future-phase" && record.Status != "unknown" {
			t.Fatalf("unrecognized phase not grouped as unknown: %+v", record)
		}
	}
	unknown := readHistory(t, storage, TaskHistoryFilter{Status: "unknown", Limit: 1, Page: 999})
	if unknown.Total != 6 || unknown.StatusCounts.Unknown != 6 || unknown.Summary.Unknown != 6 || unknown.Page != 6 || len(unknown.Records) != 1 || unknown.Records[0].Status != "unknown" {
		t.Fatalf("unknown grouping/filter/count mismatch: %+v", unknown)
	}
}

func TestTaskHistoryBatchSchedulePrefersQueuedItems(t *testing.T) {
	storage := openTest(t)
	start := time.Now().UTC()
	seedHistoryQuery(t, storage, "partial-batch", "", "running", "manual_batch", false, start)
	for index, phase := range []string{"completed", "running", "queued", "queued"} {
		account := fmt.Sprintf("item-%d", index)
		if _, err := storage.db.Exec("INSERT INTO job_items(job_id,account_id,phase) VALUES('partial-batch',?,?)", account, phase); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.db.Exec("INSERT INTO query_schedule VALUES('partial-batch',?,?,?,?)", account, queryStamp(start.Add(time.Duration(index)*time.Hour)), "private-index", "private-binding"); err != nil {
			t.Fatal(err)
		}
	}
	page := readHistory(t, storage, TaskHistoryFilter{})
	job, err := storage.Job(context.Background(), "partial-batch")
	if err != nil || page.Records[0].ScheduledAt != queryStamp(start.Add(2*time.Hour)) || page.Records[0].ScheduledAt != job.ScheduledAt || page.Records[0].Waiting != 2 || page.Records[0].Querying != 1 {
		t.Fatalf("list did not use next queued schedule: page=%+v job=%+v err=%v", page, job, err)
	}
	if _, err = storage.db.Exec("UPDATE job_items SET phase='completed'; UPDATE jobs SET phase='completed'"); err != nil {
		t.Fatal(err)
	}
	terminal := readHistory(t, storage, TaskHistoryFilter{})
	if terminal.Records[0].ScheduledAt != queryStamp(start) {
		t.Fatalf("terminal batch lost original schedule: %+v", terminal)
	}
}

func TestTaskHistorySearchUsesOnlyPublicErrorsAndModels(t *testing.T) {
	storage := openTest(t)
	stamp := time.Now().UTC()
	seedHistoryQuery(t, storage, "private-item-error", "", "failed", "manual_batch", false, stamp)
	seedHistoryQuery(t, storage, "private-job-error", "account", "failed", "manual_single", false, stamp)
	seedHistoryWake(t, storage, "private-wake-error", "unknown", "daily", stamp)
	for _, statement := range []string{
		`INSERT INTO job_items(job_id,account_id,phase,error_code) VALUES('private-item-error','batch-account','failed','Authorization: Bearer hidden-item-token')`,
		`UPDATE jobs SET error_code='Authorization: Bearer hidden-job-token' WHERE id='private-job-error'`,
		`UPDATE wake_jobs SET error_code='Authorization: Bearer hidden-wake-token',model='public-model Authorization: Bearer hidden-model-token'`,
	} {
		if _, err := storage.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, search := range []string{"hidden-item-token", "hidden-job-token", "hidden-wake-token", "hidden-model-token"} {
		page := readHistory(t, storage, TaskHistoryFilter{Search: search})
		if page.Total != 0 || page.StatusCounts.All != 0 || page.Summary.Today != 0 || len(page.Records) != 0 {
			t.Fatalf("search leaked redacted field: search=%s page=%+v", search, page)
		}
	}
	for search, want := range map[string]int{"QUOTA_QUERY_FAILED": 2, "wake_outcome_unknown": 1, "PUBLIC-MODEL": 1, "batch-account": 1} {
		page := readHistory(t, storage, TaskHistoryFilter{Search: search, Limit: 1, Page: 999})
		if page.Total != want || page.StatusCounts.All != want || page.Page != want || len(page.Records) != 1 {
			t.Fatalf("public search/count/pagination mismatch: search=%s page=%+v", search, page)
		}
	}
}

func TestTaskHistoryRejectsInvalidFilters(t *testing.T) {
	storage := openTest(t)
	for _, filter := range []TaskHistoryFilter{
		{Page: -1}, {Limit: -1}, {Limit: 101}, {Page: int(^uint(0) >> 1), Limit: 100},
		{Type: "generation"}, {Source: "internal"}, {Status: "sending"}, {Trigger: "invalid"},
		{Search: strings.Repeat("x", 257)}, {Search: "bad\x00search"}, {Search: string([]byte{255})},
		{From: "not-a-date"}, {To: "2026-10-19T00:00:00"},
		{From: "2026-10-19T00:00:00Z", To: "2026-10-19T00:00:00Z"},
		{TodayFrom: "2026-10-19T00:00:00Z"}, {TodayTo: "2026-10-20T00:00:00Z"},
		{TodayFrom: "2026-10-20T00:00:00Z", TodayTo: "2026-10-19T00:00:00Z"},
		{TodayFrom: "2026-10-19", TodayTo: "2026-10-20"},
	} {
		if _, err := storage.TaskHistory(context.Background(), filter); !errors.Is(err, ErrInvalidTaskHistoryFilter) {
			t.Fatalf("filter accepted: %+v err=%v", filter, err)
		}
	}
}
