package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type wakeAccountCase struct {
	name   string
	change string
	code   string
}

func wakeAccountCases() []wakeAccountCase {
	return []wakeAccountCase{
		{"disabled", "UPDATE accounts SET status='disabled'", "wake_account_disabled"},
		{"deleted", "UPDATE accounts SET status='removed'", "wake_account_not_found"},
		{"absent", "DELETE FROM account_bindings; DELETE FROM accounts", "wake_account_not_found"},
		{"unavailable", "UPDATE accounts SET status='unavailable'", "wake_host_unavailable"},
		{"stale inventory", "UPDATE inventory_state SET last_success_at='2000-01-01T00:00:00Z'", "wake_inventory_stale"},
		{"unsynced inventory", "UPDATE inventory_state SET last_success_at=''", "wake_inventory_stale"},
		{"failed inventory", "UPDATE inventory_state SET status='unavailable'", "wake_inventory_stale"},
		{"future inventory", "UPDATE inventory_state SET last_success_at='2100-01-01T00:00:00Z'", "wake_inventory_stale"},
		{"ambiguous", "INSERT INTO accounts(id,auth_index,status,updated_at) SELECT 'duplicate',auth_index,'available',updated_at FROM accounts", "wake_account_ambiguous"},
		{"missing binding", "DELETE FROM account_bindings", "wake_binding_missing"},
		{"empty binding", "UPDATE account_bindings SET binding=''", "wake_binding_missing"},
		{"missing index", "UPDATE accounts SET auth_index=''", "wake_binding_missing"},
		{"changed binding", "UPDATE account_bindings SET binding='replacement-private-binding'", "wake_binding_changed"},
		{"changed index", "UPDATE accounts SET auth_index='replacement-private-index'", "wake_binding_changed"},
		{"unresolved identity", "UPDATE accounts SET status='binding_unavailable',auth_index=''", "wake_account_unavailable"},
	}
}

func TestWakeAccountEvidenceAndCompatibility(t *testing.T) {
	for _, fixture := range wakeAccountCases() {
		t.Run(fixture.name, func(t *testing.T) {
			storage, accounts, at := wakeStoreFixture(t, 1)
			if _, err := storage.db.Exec(fixture.change); err != nil {
				t.Fatal(err)
			}
			err := storage.write(context.Background(), func(tx *sql.Tx) error {
				_, err := wakeBoundAccount(context.Background(), tx, WakeJob{AccountID: accounts[0].ID, Account: accounts[0]}, at)
				return err
			})
			if !errors.Is(err, ErrWakeAccount) || WakeAccountErrorCode(fmt.Errorf("wrapped: %w", err)) != fixture.code {
				t.Fatalf("err=%v code=%s", err, WakeAccountErrorCode(err))
			}
			if err.Error() != fixture.code || strings.Contains(err.Error(), accounts[0].Binding) {
				t.Fatal("unsafe or imprecise classification", err)
			}
		})
	}
	if WakeAccountErrorCode(ErrWakeAccount) != "wake_account_unavailable" || WakeAccountErrorCode(errors.New("private-binding")) != "" {
		t.Fatal("legacy or unknown error was inferred")
	}
}

func TestCreateWakeUsesWakeAccountEvidence(t *testing.T) {
	for _, fixture := range wakeAccountCases() {
		t.Run(fixture.name, func(t *testing.T) {
			storage, accounts, _ := wakeStoreFixture(t, 1)
			if _, err := storage.db.Exec(fixture.change); err != nil {
				t.Fatal(err)
			}
			_, _, err := storage.CreateWake(context.Background(), "create-classification", WakeRequest{AccountID: accounts[0].ID})
			if fixture.code == "wake_binding_changed" {
				if err != nil {
					t.Fatal("new request must pin the current binding", err)
				}
				return
			}
			if !errors.Is(err, ErrWakeAccount) || WakeAccountErrorCode(err) != fixture.code {
				t.Fatalf("err=%v code=%s", err, WakeAccountErrorCode(err))
			}
			if len(wakeStoreJobs(t, storage)) != 0 {
				t.Fatal("rejected request created a job")
			}
		})
	}
}

func TestWakePreparationAndAuthorizationUseSameAccountEvidence(t *testing.T) {
	for _, stage := range []string{"prepare", "authorize"} {
		for _, fixture := range wakeAccountCases() {
			t.Run(stage+"/"+fixture.name, func(t *testing.T) {
				storage, accounts, at := wakeStoreFixture(t, 1)
				ctx := context.Background()
				job, _, err := storage.CreateWake(ctx, "preflight-classification", WakeRequest{AccountID: accounts[0].ID})
				if err != nil {
					t.Fatal(err)
				}
				at = at.Add(time.Second)
				if stage == "authorize" {
					if _, err = storage.PrepareWake(ctx, at); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = storage.db.Exec(fixture.change); err != nil {
					t.Fatal(err)
				}
				if stage == "authorize" {
					_, err = storage.AuthorizeWake(ctx, job.ID, at)
				} else {
					_, err = storage.PrepareWake(ctx, at)
				}
				if !errors.Is(err, ErrNotFound) {
					t.Fatal("unsafe dispatch", err)
				}
				current, err := storage.Wake(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fixture.code == "wake_inventory_stale" {
					if current.Phase != "queued" || current.ErrorCode != "" || current.HoldReason != fixture.code {
						t.Fatal("transient inventory failure must hold", current)
					}
				} else if current.Phase != "failed" || current.ErrorCode != fixture.code {
					t.Fatal("imprecise preflight rejection", current)
				}
			})
		}
	}
}

func TestWakeVerificationPreservesSentOutcomeOnAccountChanges(t *testing.T) {
	for _, phase := range []string{"unknown", "verifying"} {
		for _, fixture := range wakeAccountCases() {
			t.Run(phase+"/"+fixture.name, func(t *testing.T) {
				storage, accounts, at := wakeStoreFixture(t, 1)
				ctx := context.Background()
				job, _, err := storage.CreateWake(ctx, "sent-classification", WakeRequest{AccountID: accounts[0].ID})
				if err != nil {
					t.Fatal(err)
				}
				at = at.Add(time.Second)
				if _, err = storage.PrepareWake(ctx, at); err != nil {
					t.Fatal(err)
				}
				if _, err = storage.AuthorizeWake(ctx, job.ID, at); err != nil {
					t.Fatal(err)
				}
				code := ""
				if phase == "unknown" {
					code = "wake_outcome_unknown"
				}
				if err = storage.FinishWake(ctx, job.ID, phase, code, at); err != nil {
					t.Fatal(err)
				}
				if _, err = storage.db.Exec(fixture.change); err != nil {
					t.Fatal(err)
				}
				if _, err = storage.ScheduleWakes(ctx, at.Add(2*time.Second), "boot"); err != nil {
					t.Fatal(err)
				}
				current, err := storage.Wake(ctx, job.ID)
				if err != nil || current.Phase != phase || current.ErrorCode != code || current.HoldReason != fixture.code {
					t.Fatal("sent outcome was overwritten", current, err)
				}
				if _, err = storage.PrepareWake(ctx, at.Add(3*time.Second)); !errors.Is(err, ErrNotFound) {
					t.Fatal("sent wake was offered for retransmission", err)
				}
				if err = storage.FinishWake(ctx, job.ID, "failed", fixture.code, at.Add(4*time.Second)); err != nil {
					t.Fatal(err)
				}
				current, err = storage.Wake(ctx, job.ID)
				if err != nil || current.Phase != phase || current.ErrorCode != code {
					t.Fatal("late failure replaced sent outcome", current, err)
				}
			})
		}
	}
}

func TestFinishWakePreservesPreciseAccountCodes(t *testing.T) {
	for _, fixture := range wakeAccountCases() {
		t.Run(fixture.name, func(t *testing.T) {
			storage, accounts, at := wakeStoreFixture(t, 1)
			ctx := context.Background()
			job, _, err := storage.CreateWake(ctx, "finish-classification", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = storage.db.Exec("UPDATE wake_jobs SET phase='sending' WHERE id=?", job.ID); err != nil {
				t.Fatal(err)
			}
			response := WakeResponse{Stage: "preflight", ErrorCode: fixture.code}
			if err = storage.FinishWakeWithResponse(ctx, job.ID, "failed", fixture.code, at, &response); err != nil {
				t.Fatal(err)
			}
			current, err := storage.Wake(ctx, job.ID)
			if err != nil || current.ErrorCode != fixture.code || current.Response == nil || current.Response.ErrorCode != fixture.code {
				t.Fatal("worker classification was swallowed", current, err)
			}
		})
	}
}

func TestFinishWakeNeverFailsOrRequeuesAfterSendEvidence(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		phase    string
		code     string
		response WakeResponse
	}{
		{"account error during open", "failed", "wake_account_disabled", WakeResponse{Stage: "open"}},
		{"account error during read", "failed", "wake_binding_changed", WakeResponse{Stage: "read"}},
		{"request rejection during read", "failed", "wake_request_rejected", WakeResponse{Stage: "read"}},
		{"budget error during read", "queued", "model_busy", WakeResponse{Stage: "read"}},
		{"budget error after completion", "queued", "model_busy", WakeResponse{Stage: "open", Completed: true}},
		{"HTTP evidence despite stage", "failed", "wake_account_disabled", WakeResponse{Stage: "preflight", HTTPStatus: 403}},
		{"body evidence despite stage", "failed", "wake_account_disabled", WakeResponse{Stage: "preflight", Bytes: 1}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			storage, accounts, at := wakeStoreFixture(t, 1)
			ctx := context.Background()
			job, _, err := storage.CreateWake(ctx, "sent-evidence", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = storage.db.Exec("UPDATE wake_jobs SET phase='sending' WHERE id=?", job.ID); err != nil {
				t.Fatal(err)
			}
			if err = storage.FinishWakeWithResponse(ctx, job.ID, fixture.phase, fixture.code, at, &fixture.response); err != nil {
				t.Fatal(err)
			}
			current, err := storage.Wake(ctx, job.ID)
			if err != nil || current.Phase != "unknown" || current.ErrorCode != "wake_outcome_unknown" {
				t.Fatal("delivery evidence was downgraded to unsent", current, err)
			}
			if _, err = storage.PrepareWake(ctx, at.Add(time.Second)); !errors.Is(err, ErrNotFound) {
				t.Fatal("delivery evidence allowed retransmission", err)
			}
		})
	}
}
