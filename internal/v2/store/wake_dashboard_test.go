package store

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestDashboardWakeErrorsCountOnlyFailedAndUnknown(t *testing.T) {
	storage, _, at := wakeStoreFixture(t, 1)
	for _, phase := range []string{"skipped", "completed", "cancelled", "queued", "checking", "prepared", "sending", "verifying", "failed", "unknown"} {
		seedHistoryWake(t, storage, "dashboard-"+phase, phase, "manual", at)
		if _, err := storage.db.Exec("UPDATE wake_jobs SET error_code='wake_account_unavailable' WHERE id=?", "dashboard-"+phase); err != nil {
			t.Fatal(err)
		}
	}
	dashboard, err := storage.Dashboard(context.Background())
	if err != nil || dashboard.Errors != 2 {
		t.Fatal("only failed and unknown wakes should count as errors", dashboard.Errors, err)
	}
	found := false
	for _, event := range dashboard.RecentEvents {
		if event.Kind == "model_wake" {
			want := "info"
			if event.Phase == "failed" || event.Phase == "unknown" {
				want = "error"
			}
			if event.Severity != want {
				t.Fatal("wrong wake event severity", event)
			}
			if event.Phase == "skipped" {
				found = true
				if event.Detail != "模型唤醒已跳过，无需重复唤醒" {
					t.Fatal("skip was not described as normal", event)
				}
			}
		}
	}
	if !found {
		t.Fatal("skipped event missing")
	}
}

func TestWakeActiveWindowSkipHasReasonWithoutError(t *testing.T) {
	for _, stage := range []string{"prepare", "authorize"} {
		t.Run(stage, func(t *testing.T) {
			storage, accounts, at := wakeStoreFixture(t, 1)
			ctx := context.Background()
			job, _, err := storage.CreateWake(ctx, "active-window", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Second)
			if stage == "authorize" {
				if _, err = storage.PrepareWake(ctx, at); err != nil {
					t.Fatal(err)
				}
			}
			wakeStoreQuota(t, storage, accounts[0], at, core.WindowActive, at.Add(5*time.Hour), at.Add(24*time.Hour))
			if stage == "authorize" {
				_, _ = storage.AuthorizeWake(ctx, job.ID, at)
			} else {
				_, _ = storage.PrepareWake(ctx, at)
			}
			current, err := storage.Wake(ctx, job.ID)
			if err != nil || current.Phase != "skipped" || current.ErrorCode != "" || current.HoldReason != "window_already_active" {
				t.Fatal("active window skip treated as error", current, err)
			}
			var storedCode string
			if err = storage.db.QueryRow("SELECT error_code FROM wake_jobs WHERE id=?", job.ID).Scan(&storedCode); err != nil || storedCode != "" {
				t.Fatal("new skip persisted an error", storedCode, err)
			}
			dashboard, err := storage.Dashboard(ctx)
			if err != nil || dashboard.Errors != 0 {
				t.Fatal("normal skip increased dashboard errors", dashboard.Errors, err)
			}
		})
	}
}
