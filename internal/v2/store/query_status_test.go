package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func queryStatusRun(t *testing.T, storage *Store) (Run, Job) {
	t.Helper()
	ctx := context.Background()
	account := Account{ID: "status-account", AuthIndex: "status-index", Binding: "status-binding", Status: "available"}
	if err := storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	job := createTest(t, storage, "status-query", account.ID)
	if err := storage.Expand(ctx, job, []Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := storage.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return run, job
}

func observedQueryStatus(run Run, status string) *Account {
	return &Account{ID: run.Account.ID, AuthIndex: run.Account.AuthIndex, Binding: run.Account.Binding, Status: status, StatusObservedAt: now()}
}

func TestQueryCommitsHostStatusWithSuccessAndFailure(t *testing.T) {
	for _, code := range []string{"", "quota_authentication_failed", "quota_transport_failed"} {
		t.Run(code, func(t *testing.T) {
			storage := openTest(t)
			run, job := queryStatusRun(t, storage)
			ctx := context.Background()
			before, err := storage.AccountPage(ctx, "", 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			observed := observedQueryStatus(run, "disabled")
			var raw json.RawMessage
			if code == "" {
				raw = json.RawMessage(`{"permission":"allowed"}`)
			}
			if err = storage.FinishWithStatus(ctx, run, raw, code, observed); err != nil {
				t.Fatal(err)
			}
			page, err := storage.AccountPage(ctx, "", 10, nil)
			if err != nil || len(page.Accounts) != 1 || page.Accounts[0].Status != "disabled" || page.Accounts[0].DisplayStatus != "disabled" || page.Accounts[0].StatusObservedAt != observed.StatusObservedAt || page.AccountsRevision <= before.AccountsRevision {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			result, err := storage.Job(ctx, job.ID)
			if err != nil || len(result.Items) != 1 || result.Items[0].ErrorCode != code || code != "" && len(page.Accounts[0].Quota) != 0 {
				t.Fatalf("job=%+v page=%+v err=%v", result, page, err)
			}
			account := run.Account
			account.Status = "unavailable"
			if err = storage.SyncInventory(ctx, []Account{account}); err != nil {
				t.Fatal(err)
			}
			if err = storage.FinishWithStatus(ctx, run, raw, code, observed); err != nil {
				t.Fatal(err)
			}
			page, err = storage.AccountPage(ctx, "", 10, nil)
			if err != nil || page.Accounts[0].Status != "unavailable" {
				t.Fatal("replayed outcome overwrote a newer status", page, err)
			}
		})
	}
}

func TestQueryStatusRejectsStaleOrUnboundEvidence(t *testing.T) {
	for _, scenario := range []string{"newer inventory", "changed binding", "removed", "wrong id", "wrong index", "wrong binding", "missing binding", "invalid status", "invalid time", "future time", "missing observation"} {
		t.Run(scenario, func(t *testing.T) {
			storage := openTest(t)
			run, _ := queryStatusRun(t, storage)
			ctx := context.Background()
			observed := observedQueryStatus(run, "disabled")
			wanted := "available"
			switch scenario {
			case "newer inventory", "changed binding", "removed":
				accounts := []Account{run.Account}
				if scenario == "changed binding" {
					accounts[0].Binding = "replacement-binding"
				}
				if scenario == "removed" {
					accounts = nil
					wanted = "removed"
				}
				if err := storage.SyncInventory(ctx, accounts); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed binding" {
					observed.StatusObservedAt = now()
				}
			case "wrong id":
				observed.ID = "other-account"
			case "wrong index":
				observed.AuthIndex = "other-index"
			case "wrong binding":
				observed.Binding = "other-binding"
			case "missing binding":
				observed.Binding = ""
			case "invalid status":
				observed.Status = "removed"
			case "invalid time":
				observed.StatusObservedAt = "invalid"
			case "future time":
				observed.StatusObservedAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			case "missing observation":
				observed = nil
			}
			if err := storage.FinishWithStatus(ctx, run, nil, "quota_transport_failed", observed); err != nil {
				t.Fatal(err)
			}
			accounts, err := storage.Accounts(ctx, "", 10)
			if err != nil || len(accounts) != 1 || accounts[0].Status != wanted {
				t.Fatalf("accounts=%+v err=%v", accounts, err)
			}
		})
	}
}

func TestQueryStatusDoesNotInferAvailabilityFromSuccessfulQuota(t *testing.T) {
	storage := openTest(t)
	run, _ := queryStatusRun(t, storage)
	ctx := context.Background()
	account := run.Account
	account.Status = "disabled"
	if err := storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	page, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || page.Accounts[0].Status != "disabled" {
		t.Fatal("quota success re-enabled the host account", page, err)
	}
}

func TestQueryStatusDoesNotClearExistingQuotaOnFailure(t *testing.T) {
	storage := openTest(t)
	run, _ := queryStatusRun(t, storage)
	ctx := context.Background()
	account := run.Account
	account.Status = "disabled"
	if err := storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	raw := `{"permission":"allowed","five_hour":{"Remaining":{"Value":0,"Unit":"percent"}}}`
	if _, err := storage.db.ExecContext(ctx, "UPDATE accounts SET quota=?,observed_at=? WHERE id=?", raw, now(), account.ID); err != nil {
		t.Fatal(err)
	}
	observed := observedQueryStatus(run, "available")
	if err := storage.FinishWithStatus(ctx, run, nil, "quota_authentication_failed", observed); err != nil {
		t.Fatal(err)
	}
	page, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || len(page.Accounts) != 1 || page.Accounts[0].Status != "available" || page.Accounts[0].DisplayStatus != "quota_exhausted" || string(page.Accounts[0].Quota) != raw {
		t.Fatal("host recovery erased quota evidence or authentication failure guessed quota", page, err)
	}
}
