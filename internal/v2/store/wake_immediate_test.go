package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

func TestManualWakeReadsHaveNoRandomDelayEvenWithDelayedExistingQuery(t *testing.T) {
	for _, phase := range []string{"queued", "verifying", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			st, accounts, at := wakeStoreFixture(t, 1)
			settings, _ := st.QuerySettings(ctx)
			settings.Config.Delay = policy.Delay{Min: 60, Max: 60}
			if _, err := st.SaveQuerySettings(ctx, settings.Revision, settings.Config); err != nil {
				t.Fatal(err)
			}
			job, _, err := st.CreateWake(ctx, "immediate", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			job.Account, job.Reason, job.Phase = accounts[0], "manual", phase
			err = st.write(ctx, func(tx *sql.Tx) error {
				// A previously queued batch must not impose its delay on a manual wake.
				if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,expanded,created_at,updated_at) VALUES('delayed-batch','delayed-batch','batch',?,'running',1,?,?)", accounts[0].ID, queryStamp(at), queryStamp(at)); err != nil {
					return err
				}
				if err := insertQueryItem(ctx, tx, "delayed-batch", accounts[0], QueryPlan{Min: 60, Max: 60}, at); err != nil {
					return err
				}
				_, err := enqueueWakeRead(ctx, tx, &job, at)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			var scheduled string
			if err := st.db.QueryRow("SELECT scheduled_at FROM query_schedule WHERE job_id=?", job.QueryJobID).Scan(&scheduled); err != nil || scheduled != queryStamp(at) {
				t.Fatal(scheduled, err)
			}
		})
	}
}

func TestDifferentWakeAccountsShareBoundedBudgetAndOnlyBlockThemselves(t *testing.T) {
	ctx := context.Background()
	st, accounts, at := wakeStoreFixture(t, ModelConcurrency+1)
	for i, account := range accounts {
		if _, _, err := st.CreateWake(ctx, fmt.Sprint(i), WakeRequest{AccountID: account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	at = at.Add(time.Second)
	wakeStoreActivity(t, st, at, 1)
	for i := 0; i < ModelConcurrency; i++ {
		job, err := st.PrepareWake(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.AuthorizeWake(ctx, job.ID, at); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.CreateWake(ctx, "duplicate-"+job.ID, WakeRequest{AccountID: job.AccountID}); !errors.Is(err, ErrBusy) {
			t.Fatal("same account was accepted twice", err)
		}
	}
	if _, err := st.PrepareWake(ctx, at); !errors.Is(err, ErrNotFound) {
		t.Fatal("physical budget exceeded", err)
	}
	jobs := wakeStoreJobs(t, st)
	if err := st.FinishWake(ctx, jobs[0].ID, "unknown", "wake_outcome_unknown", at); err != nil {
		t.Fatal(err)
	}
	wakeStoreActivity(t, st, at.Add(time.Second), 1)
	if _, err := st.PrepareWake(ctx, at.Add(time.Second)); err != nil {
		t.Fatal("unknown account blocked a different account", err)
	}
}

func TestWakeResponseQuotaUpdatesAndVerifiesWithoutAnotherQuery(t *testing.T) {
	for _, scenario := range []string{"active", "partial", "placeholder", "stale", "replaced", "newer-inactive"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			st, accounts, at := wakeStoreFixture(t, 1)
			job, _, err := st.CreateWake(ctx, "response-quota", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Second)
			wakeStoreActivity(t, st, at, 1)
			if _, err = st.PrepareWake(ctx, at); err != nil {
				t.Fatal(err)
			}
			if _, err = st.AuthorizeWake(ctx, job.ID, at); err != nil {
				t.Fatal(err)
			}
			proofAt := at.Add(time.Second)
			raw := []byte(`{"rate_limits":{"primary":{"used_percent":1,"window_minutes":300,"reset_after_seconds":17900},"secondary":{"used_percent":3,"window_minutes":10080,"reset_after_seconds":500000}}}`)
			snapshot, err := quota.Normalize(raw, nil, accounts[0].Binding, "cpa_websocket_quota", proofAt, proofAt)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "partial":
				snapshot.Weekly = nil
			case "placeholder":
				snapshot.FiveHour.State = core.WindowUnknown
			case "stale":
				snapshot.ReadStartedAt = at.Add(-time.Second)
			case "replaced":
				if _, err = st.db.Exec("UPDATE account_bindings SET binding='replacement' WHERE account_id=?", accounts[0].ID); err != nil {
					t.Fatal(err)
				}
			case "newer-inactive":
				wakeStoreQuota(t, st, accounts[0], proofAt.Add(time.Second), core.WindowInactive, proofAt.Add(5*time.Hour), proofAt.Add(7*24*time.Hour))
			}
			response := WakeResponse{Stage: "completed", Completed: true, HTTPStatus: 200, Quota: &snapshot}
			if err = st.FinishWakeWithResponse(ctx, job.ID, "verifying", "", proofAt.Add(2*time.Second), &response); err != nil {
				t.Fatal(err)
			}
			result, err := st.Wake(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			verified := scenario == "active" || scenario == "partial"
			if (result.Phase == "completed") != verified {
				t.Fatal(scenario, result)
			}
			if verified {
				var stored string
				if err = st.db.QueryRow("SELECT quota FROM accounts WHERE id=?", accounts[0].ID).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				var updated quota.Snapshot
				if json.Unmarshal([]byte(stored), &updated) != nil || updated.FiveHour.Remaining.Value != 99 || updated.FiveHour.ResetAt != snapshot.FiveHour.ResetAt || updated.Weekly == nil {
					t.Fatal(stored)
				}
				var queries int
				if err = st.db.QueryRow("SELECT count(*) FROM jobs").Scan(&queries); err != nil || queries != 0 {
					t.Fatal("unnecessary verification query", queries, err)
				}
			}
		})
	}
}
