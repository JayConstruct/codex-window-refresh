package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

func TestWakeWorkersExecuteDifferentAccountsAtTheSameTime(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	source := &workerWakeSource{}
	source.execute = func(ctx context.Context, account store.Account, _, _ string) error {
		entered <- account.ID
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service, storage := prepareWakeWorker(t, source)
	source.catalog = append(source.catalog, store.Account{ID: "second-wake-account", AuthIndex: "second-index", Binding: "second-binding", Status: "available"})
	if err := storage.SyncInventory(context.Background(), source.catalog); err != nil {
		t.Fatal(err)
	}
	first := acceptWake(t, service, "parallel-first")
	response := request(service, "POST", "/api/v2/wake-jobs", `{"account_id":"second-wake-account"}`, "parallel-second")
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	var second store.WakeJob
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("different accounts did not enter execution concurrently", seen)
		}
	}
	for _, id := range []string{first.ID, second.ID} {
		job, err := storage.Wake(ctx, id)
		if err != nil || job.Phase != "sending" {
			t.Fatal(job, err)
		}
	}
	close(release)
	awaitWake(t, storage, first.ID, "verifying")
	awaitWake(t, storage, second.ID, "verifying")
}
