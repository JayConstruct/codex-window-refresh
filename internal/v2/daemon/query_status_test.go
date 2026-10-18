package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type queryStatusSource struct {
	queries atomic.Int32
	code    string
}

func (source *queryStatusSource) Accounts(context.Context) ([]store.Account, error) {
	return nil, errors.New("unexpected_inventory_request")
}

func (source *queryStatusSource) Query(context.Context, store.Account) (json.RawMessage, error) {
	return nil, errors.New("legacy_query_must_not_be_called")
}

func (source *queryStatusSource) QueryWithStatus(_ context.Context, account store.Account) (json.RawMessage, *store.Account, error) {
	source.queries.Add(1)
	account.Status = "disabled"
	account.StatusObservedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if source.code != "" {
		return nil, &account, errors.New(source.code)
	}
	return json.RawMessage(`{"permission":"allowed"}`), &account, nil
}

func TestQueryWorkerPublishesHostStatusEvenWhenQuotaFails(t *testing.T) {
	for _, code := range []string{"", "quota_authentication_failed"} {
		t.Run(code, func(t *testing.T) {
			storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			account := store.Account{ID: "runtime-status", AuthIndex: "index", Binding: "binding", Status: "available"}
			if err = storage.SyncInventory(ctx, []store.Account{account}); err != nil {
				t.Fatal(err)
			}
			source := &queryStatusSource{code: code}
			service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			id := accepted(t, service, account.ID, "status-worker")
			job, err := storage.Job(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = storage.Expand(ctx, job, []store.Account{account}); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); service.worker(ctx, 0) }()
			defer func() { cancel(); <-done }()
			until(t, func() bool {
				result, readErr := storage.Job(ctx, id)
				return readErr == nil && len(result.Items) == 1 && (result.Items[0].Phase == "completed" || result.Items[0].Phase == "failed")
			})
			response := request(service, "GET", "/api/v2/accounts", "", "")
			var page store.AccountPage
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Accounts) != 1 || page.Accounts[0].Status != "disabled" || page.Accounts[0].StatusObservedAt == "" || source.queries.Load() != 1 {
				t.Fatal("query did not update status", response.Code, response.Body.String(), source.queries.Load())
			}
			job, err = storage.Job(ctx, id)
			if err != nil || job.Items[0].ErrorCode != code {
				t.Fatal(job, err)
			}
		})
	}
}
