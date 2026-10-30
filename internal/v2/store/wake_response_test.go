package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWakeResponseIsDurableSanitizedAndDoesNotChangeUnknownProtection(t *testing.T) {
	storage, accounts, at := wakeStoreFixture(t, 1)
	ctx := context.Background()
	job, _, err := storage.CreateWake(ctx, "response-record", WakeRequest{AccountID: accounts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.db.Exec("UPDATE wake_jobs SET phase='sending' WHERE id=?", job.ID); err != nil {
		t.Fatal(err)
	}
	response := WakeResponse{Stage: "read", HTTPStatus: 429, Event: "response.failed", Status: "failed", UpstreamErrorCode: "rate_limit_exceeded", UpstreamErrorType: "rate_limit_error", ErrorCode: "wake_delivery_unknown", ErrorMessage: "slow down Authorization: Bearer private-access-token api_key=another-secret https://user:password@proxy.example/a", ResponseText: "email test@example.com sk-secret-token", Bytes: 512}
	if err = storage.FinishWakeWithResponse(ctx, job.ID, "unknown", "wake_outcome_unknown", at, &response); err != nil {
		t.Fatal(err)
	}
	saved, err := storage.Wake(ctx, job.ID)
	if err != nil || saved.Response == nil || saved.Phase != "unknown" || saved.Response.HTTPStatus != 429 || saved.Response.UpstreamErrorCode != "rate_limit_exceeded" || saved.Response.RecordedAt == "" {
		t.Fatal(saved, err)
	}
	raw, _ := json.Marshal(saved.Response)
	for _, secret := range []string{"private-access-token", "another-secret", "user:password", "test@example.com", "sk-secret-token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("response leaked a secret", secret)
		}
	}
	page, err := storage.WakeJobs(ctx, 20)
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].Response == nil {
		t.Fatal(page, err)
	}
	response.ErrorMessage = "late overwrite"
	if err = storage.FinishWakeWithResponse(ctx, job.ID, "verifying", "", at.Add(time.Second), &response); err != nil {
		t.Fatal(err)
	}
	saved, err = storage.Wake(ctx, job.ID)
	if err != nil || saved.Phase != "unknown" || strings.Contains(saved.Response.ErrorMessage, "late overwrite") {
		t.Fatal(saved, err)
	}
	if _, _, err = storage.CreateWake(ctx, "new-identity", WakeRequest{AccountID: accounts[0].ID}); err != ErrBusy {
		t.Fatal("audit read unlocked unknown delivery", err)
	}
	if err = storage.RecoverWakes(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err = storage.Wake(ctx, job.ID)
	if err != nil || saved.Response.HTTPStatus != 429 {
		t.Fatal("recovery erased existing response", saved, err)
	}
}

func TestWakeResponseMigrationFromEightPreservesJobsAndRecordsInterruptedSend(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	storage, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := now()
	if _, err = storage.db.Exec("INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at) VALUES('interrupted','account','index','binding','manual','cycle','sending',?,?,?)", stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.db.Exec("DROP TABLE wake_responses; UPDATE schema_version SET version=8"); err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var version int
	if err = storage.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 12 {
		t.Fatal(version, err)
	}
	if err = storage.RecoverWakes(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := storage.Wake(ctx, "interrupted")
	if err != nil || job.Phase != "unknown" || job.Response == nil || job.Response.Stage != "interrupted" || job.Response.ErrorCode != "wake_interrupted" || job.Response.RecordedAt == "" {
		t.Fatal(job, err)
	}
}

func TestWakeResponseBoundsTextAndClassifiesArbitraryErrors(t *testing.T) {
	response := NormalizeWakeResponse(WakeResponse{Stage: "raw secret", HTTPStatus: 999, ErrorCode: "private error", Event: "private-token value", ResponseText: strings.Repeat("中文", 2000), Bytes: 2 << 20})
	if response.Stage != "interrupted" || response.HTTPStatus != 0 || response.ErrorCode != "wake_outcome_unknown" || response.Event != "" || !response.Truncated || len(response.ResponseText) > 2048 || response.Bytes > 1<<20 {
		t.Fatal(response)
	}
}

func TestWakeRecordedFailureSurvivesDatabaseReopen(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	storage, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	if _, err = storage.db.Exec("INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,scheduled_at,created_at,updated_at) VALUES('durable-response','account','index','binding','manual','cycle','sending',?,?,?)", stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	response := WakeResponse{Stage: "read", HTTPStatus: 403, ErrorMessage: "Model not permitted", ErrorCode: "wake_delivery_unknown", Bytes: 128}
	if err = storage.FinishWakeWithResponse(context.Background(), "durable-response", "unknown", "wake_outcome_unknown", time.Now().UTC(), &response); err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err = storage.RecoverWakes(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := storage.Wake(context.Background(), "durable-response")
	if err != nil || job.Phase != "unknown" || job.Response == nil || job.Response.HTTPStatus != 403 || job.Response.ErrorMessage != "Model not permitted" || job.Revision == 0 {
		t.Fatal(job, err)
	}
}
