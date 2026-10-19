package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

func wakeStoreFixture(t *testing.T, count int) (*Store, []Account, time.Time) {
	t.Helper()
	storage, err := Open(privateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	at := time.Now().UTC()
	accounts := []Account{}
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("wake-%03d", index)
		accounts = append(accounts, Account{ID: id, AuthIndex: "index-" + id, Binding: "binding-" + id, Status: "available"})
	}
	if err = storage.SyncInventory(context.Background(), accounts); err != nil {
		t.Fatal(err)
	}
	model := policy.DefaultModel()
	model.DefaultModel = "wake-model"
	model.WeeklyModel = "weekly-model"
	model.WeeklyPrompt = "weekly prompt"
	if _, err = storage.SaveModelSettings(context.Background(), 0, model); err != nil {
		t.Fatal(err)
	}
	queries := policy.DefaultQuery()
	queries.Delay = policy.Delay{}
	if _, err = storage.SaveQuerySettings(context.Background(), 0, queries); err != nil {
		t.Fatal(err)
	}
	wakeStoreActivity(t, storage, at, 1)
	for _, account := range accounts {
		wakeStoreQuota(t, storage, account, at, core.WindowExpired, at.Add(-time.Second), at.Add(24*time.Hour))
	}
	return storage, accounts, at
}

func wakeStoreActivity(t *testing.T, storage *Store, at time.Time, users int) {
	t.Helper()
	if _, err := storage.db.Exec("UPDATE event_state SET bridge_instance='bridge',host_process='host',connected=1,status='observing',last_seen=? WHERE singleton=1", queryStamp(at)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec("DELETE FROM activity_users"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < users; index++ {
		if _, err := storage.db.Exec("INSERT INTO activity_users VALUES(?,?)", fmt.Sprintf("user-%d", index), queryStamp(at)); err != nil {
			t.Fatal(err)
		}
	}
}

func wakeStoreQuota(t *testing.T, storage *Store, account Account, at time.Time, state core.WindowState, reset, weeklyReset time.Time) quota.Snapshot {
	t.Helper()
	permission := core.PermissionAllowed
	snapshot := quota.Snapshot{Binding: account.Binding, Source: "cpa_wham_usage", ObservedAt: at, ReadStartedAt: at, Permission: &permission,
		FiveHour: &core.WindowEvidence{Kind: core.FiveHour, State: state, ResetAt: reset, ConfirmedAt: at, Source: "cpa_wham_usage", Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}},
		Weekly:   &core.WindowEvidence{Kind: core.Weekly, State: core.WindowActive, ResetAt: weeklyReset, ConfirmedAt: at, Source: "cpa_wham_usage", Remaining: &core.QuotaAmount{Value: 99, Unit: "percent"}}}
	wakeStoreSnapshot(t, storage, account, snapshot)
	return snapshot
}

func wakeStoreSnapshot(t *testing.T, storage *Store, account Account, snapshot quota.Snapshot) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.db.Exec("UPDATE accounts SET quota=?,observed_at=? WHERE id=?", string(raw), queryStamp(snapshot.ObservedAt), account.ID); err != nil {
		t.Fatal(err)
	}
}

func wakeStoreEnable(t *testing.T, storage *Store, config policy.WakeConfig) {
	t.Helper()
	current, err := storage.WakeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.SaveWakeSettings(context.Background(), current.Revision, config); err != nil {
		t.Fatal(err)
	}
}

func wakeStoreJobs(t *testing.T, storage *Store) []WakeJob {
	t.Helper()
	page, err := storage.WakeJobs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return page.Jobs
}

func TestWakeStartupDeadlinesPersistAndLiveResetIsImmediate(t *testing.T) {
	ctx := context.Background()
	storage, _, at := wakeStoreFixture(t, 2)
	config := policy.DefaultWake()
	config.Enabled, config.FiveHour = true, true
	config.Delay = policy.Delay{Min: 20, Max: 20}
	config.AllowedHours = []string{}
	wakeStoreEnable(t, storage, config)
	if count, err := storage.ScheduleWakes(ctx, at, "first-boot"); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, job := range wakeStoreJobs(t, storage) {
		if job.ScheduledAt != queryStamp(at.Add(20*time.Second)) {
			t.Fatal("startup was not staggered", job)
		}
	}
	config.Delay = policy.Delay{Min: 90, Max: 90}
	wakeStoreEnable(t, storage, config)
	if count, err := storage.ScheduleWakes(ctx, at.Add(time.Second), "new-boot"); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	for _, job := range wakeStoreJobs(t, storage) {
		if job.ScheduledAt != queryStamp(at.Add(20*time.Second)) {
			t.Fatal("deadline redrawn", job)
		}
	}
	other, liveAccounts, liveAt := wakeStoreFixture(t, 1)
	account := liveAccounts[0]
	wakeStoreQuota(t, other, account, liveAt, core.WindowActive, liveAt.Add(time.Second), liveAt.Add(24*time.Hour))
	wakeStoreEnable(t, other, config)
	if count, err := other.ScheduleWakes(ctx, liveAt, "live"); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	observed := liveAt.Add(2 * time.Second)
	wakeStoreActivity(t, other, observed, 1)
	wakeStoreQuota(t, other, account, observed, core.WindowUnknown, observed.Add(5*time.Hour), observed.Add(24*time.Hour))
	if count, err := other.ScheduleWakes(ctx, observed, "live"); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	jobs := wakeStoreJobs(t, other)
	if len(jobs) != 1 || jobs[0].ScheduledAt != queryStamp(observed) {
		t.Fatal("live reset was randomized", jobs)
	}
	prepared, err := other.PrepareWake(ctx, observed)
	if err != nil || prepared.Account.ID != account.ID {
		t.Fatal(prepared, err)
	}
	authorized, err := other.AuthorizeWake(ctx, prepared.ID, observed)
	if err != nil || authorized.Phase != "sending" {
		t.Fatal(authorized, err)
	}
}

func TestWakeAtomicCapacityReservationAndWeeklyMergePriority(t *testing.T) {
	ctx := context.Background()
	storage, _, at := wakeStoreFixture(t, 2)
	config := policy.DefaultWake()
	config.Enabled, config.FiveHour = true, true
	config.Delay = policy.Delay{}
	config.AllowedHours = []string{}
	wakeStoreEnable(t, storage, config)
	if _, err := storage.ScheduleWakes(ctx, at, "boot"); err != nil {
		t.Fatal(err)
	}
	first, err := storage.PrepareWake(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.PrepareWake(ctx, at); !errors.Is(err, ErrNotFound) {
		t.Fatal("capacity oversubscribed", err)
	}
	if _, err = storage.AuthorizeWake(ctx, first.ID, at); err != nil {
		t.Fatal(err)
	}
	if err = storage.FinishWake(ctx, first.ID, "unknown", "wake_outcome_unknown", at); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.PrepareWake(ctx, at.Add(2*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown reservation ignored", err)
	}
	weekly, accounts, weekAt := wakeStoreFixture(t, 1)
	account := accounts[0]
	wakeStoreActivity(t, weekly, weekAt, 0)
	wakeStoreQuota(t, weekly, account, weekAt, core.WindowActive, weekAt.Add(time.Second), weekAt.Add(time.Second))
	config.Weekly = true
	config.AllowedHours = []string{"00:00-00:01"}
	wakeStoreEnable(t, weekly, config)
	if _, err = weekly.ScheduleWakes(ctx, weekAt, "weekly"); err != nil {
		t.Fatal(err)
	}
	observed := weekAt.Add(2 * time.Second)
	wakeStoreActivity(t, weekly, observed, 0)
	wakeStoreQuota(t, weekly, account, observed, core.WindowExpired, weekAt.Add(time.Second), observed.Add(7*24*time.Hour))
	if count, err := weekly.ScheduleWakes(ctx, observed, "weekly"); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	jobs := wakeStoreJobs(t, weekly)
	if len(jobs) != 1 || jobs[0].Reason != "weekly_reset" {
		t.Fatal(jobs)
	}
	prepared, err := weekly.PrepareWake(ctx, observed)
	if err != nil || prepared.Model != "weekly-model" || prepared.Prompt != "weekly prompt" {
		t.Fatal(prepared, err)
	}
	var merged int
	if err = weekly.db.QueryRow("SELECT count(*) FROM wake_cycles WHERE job_id=?", prepared.ID).Scan(&merged); err != nil || merged != 2 {
		t.Fatal("reset reasons not merged", merged, err)
	}
}

func TestWakeFinalAuthorizationRechecksSettingsBindingAndActivity(t *testing.T) {
	for _, change := range []string{"model", "binding", "activity", "quota"} {
		t.Run(change, func(t *testing.T) {
			storage, accounts, at := wakeStoreFixture(t, 1)
			ctx := context.Background()
			job, _, err := storage.CreateWake(ctx, "manual", WakeRequest{AccountID: accounts[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			at = time.Now().UTC()
			prepared, err := storage.PrepareWake(ctx, at.Add(time.Millisecond))
			if err != nil {
				t.Fatal(prepared, err)
			}
			switch change {
			case "model":
				settings, _ := storage.ModelSettings(ctx)
				settings.Config.DefaultPrompt = "changed after preparation"
				if _, err = storage.SaveModelSettings(ctx, settings.Revision, settings.Config); err != nil {
					t.Fatal(err)
				}
			case "binding":
				accounts[0].Binding = "replacement"
				if err = storage.SyncInventory(ctx, accounts); err != nil {
					t.Fatal(err)
				}
			case "activity":
				if _, err = storage.db.Exec("UPDATE event_state SET status='gap'"); err != nil {
					t.Fatal(err)
				}
			case "quota":
				wakeStoreQuota(t, storage, accounts[0], at.Add(time.Millisecond), core.WindowActive, at.Add(time.Hour), at.Add(24*time.Hour))
			}
			if _, err = storage.AuthorizeWake(ctx, job.ID, at.Add(2*time.Millisecond)); !errors.Is(err, ErrNotFound) {
				t.Fatal("changed state authorized", err)
			}
			current, _ := storage.Wake(ctx, job.ID)
			if current.Phase == "sending" {
				t.Fatal(current)
			}
		})
	}
}

func TestWakeUnknownRecoverySuppressesAllModelActionsAndOnlyFreshProofResolves(t *testing.T) {
	ctx := context.Background()
	storage, accounts, at := wakeStoreFixture(t, 1)
	account := accounts[0]
	job, _, err := storage.CreateWake(ctx, "same", WakeRequest{AccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	at = time.Now().UTC()
	prepared, err := storage.PrepareWake(ctx, at.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AuthorizeWake(ctx, prepared.ID, at.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = storage.RecoverWakes(ctx); err != nil {
		t.Fatal(err)
	}
	unknown, _ := storage.Wake(ctx, job.ID)
	if unknown.Phase != "unknown" {
		t.Fatal(unknown)
	}
	if _, _, err = storage.CreateWake(ctx, "different", WakeRequest{AccountID: account.ID}); !errors.Is(err, ErrBusy) {
		t.Fatal("unknown wake bypassed", err)
	}
	if _, _, err = storage.CreateGeneration(ctx, "candidate", GenerationRequest{AccountID: account.ID, Model: "model", Count: 1}); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal("candidate bypassed unknown guard", err)
	}
	proofAt := time.Now().UTC().Add(10 * time.Millisecond)
	wakeStoreActivity(t, storage, proofAt, 1)
	snapshot := wakeStoreQuota(t, storage, account, proofAt, core.WindowActive, proofAt.Add(time.Hour), proofAt.Add(24*time.Hour))
	snapshot.Fields = map[string]quota.Stamp{"five_hour": {Source: "cpa_wham_usage", ObservedAt: proofAt, ReadStartedAt: at.Add(-time.Hour)}}
	wakeStoreSnapshot(t, storage, account, snapshot)
	if _, err = storage.ScheduleWakes(ctx, proofAt, "new-boot"); err != nil {
		t.Fatal(err)
	}
	current, _ := storage.Wake(ctx, job.ID)
	if current.Phase != "unknown" {
		t.Fatal("old read resolved unknown", current)
	}
	wakeStoreQuota(t, storage, account, proofAt.Add(2*time.Second), core.WindowActive, proofAt.Add(time.Hour), proofAt.Add(24*time.Hour))
	wakeStoreActivity(t, storage, proofAt.Add(2*time.Second), 1)
	if _, err = storage.ScheduleWakes(ctx, proofAt.Add(2*time.Second), "new-boot"); err != nil {
		t.Fatal(err)
	}
	current, _ = storage.Wake(ctx, job.ID)
	if current.Phase != "completed" || current.ErrorCode != "window_verified" {
		t.Fatal(current)
	}
}

func TestWakeSharedGenerationBudgetAndPendingFairness(t *testing.T) {
	ctx := context.Background()
	storage, accounts, at := wakeStoreFixture(t, 34)
	for index, account := range accounts {
		if _, _, err := storage.CreateWake(ctx, fmt.Sprintf("manual-%d", index), WakeRequest{AccountID: account.ID}); err != nil {
			t.Fatal(err)
		}
		if index < 33 {
			if _, err := storage.db.Exec("INSERT INTO activity_requests VALUES('bridge',?,? ,?,'active',?,?)", fmt.Sprintf("request-%d", index), fmt.Sprintf("identity-%d", index), account.ID, queryStamp(at), queryStamp(at)); err != nil {
				t.Fatal(err)
			}
		}
	}
	firstAt := time.Now().UTC().Add(time.Millisecond)
	wakeStoreActivity(t, storage, firstAt, 1)
	if _, err := storage.PrepareWake(ctx, firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	prepared, err := storage.PrepareWake(ctx, firstAt)
	if err != nil || prepared.Account.ID != accounts[33].ID {
		t.Fatal("busy prefix starved eligible account", prepared, err)
	}
	if _, _, err = storage.CreateGeneration(ctx, "candidate", GenerationRequest{AccountID: accounts[0].ID, Model: "model", Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ClaimGeneration(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal("generation exceeded shared physical slot", err)
	}
}

func TestWakeDailyTargetAndWindowArePersistentAndSeparateFromQueryRules(t *testing.T) {
	ctx := context.Background()
	storage, _, at := wakeStoreFixture(t, 2)
	wakeStoreActivity(t, storage, at, 0)
	config := policy.DefaultWake()
	config.Enabled, config.DailyEnabled = true, true
	config.Timezone = "UTC"
	config.DailyWindow = "00:00-24:00"
	config.AllowedHours = []string{}
	config.DailyTarget = 1
	wakeStoreEnable(t, storage, config)
	before, err := storage.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := storage.ScheduleWakes(ctx, at, "daily"); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	jobs := wakeStoreJobs(t, storage)
	date, start, end, _ := config.DailyBounds(at)
	for _, job := range jobs {
		due, err := time.Parse(time.RFC3339Nano, job.ScheduledAt)
		if err != nil || job.Reason != "daily" || job.Cycle != date || due.Before(at) || due.Before(start) || due.After(end) {
			t.Fatal(job, err)
		}
	}
	if _, err = storage.db.Exec("UPDATE wake_jobs SET scheduled_at=?", queryStamp(at)); err != nil {
		t.Fatal(err)
	}
	first, err := storage.PrepareWake(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.PrepareWake(ctx, at); !errors.Is(err, ErrNotFound) {
		t.Fatal("daily target oversubscribed", err)
	}
	if _, err = storage.ScheduleWakes(ctx, at, "new-boot"); err != nil {
		t.Fatal(err)
	}
	if len(wakeStoreJobs(t, storage)) != 2 {
		t.Fatal("restart duplicated daily intents")
	}
	config.DailyTarget = 0
	wakeStoreEnable(t, storage, config)
	current, _ := storage.Wake(ctx, first.ID)
	if current.Phase != "cancelled" {
		t.Fatal("zero daily target did not cancel unsent task", current)
	}
	after, err := storage.QuerySettings(ctx)
	if err != nil || after.Revision != before.Revision {
		t.Fatal("wake settings altered query settings", after, err)
	}
}

func TestWakePreflightWillNotReuseQueryStartedBeforeReadFence(t *testing.T) {
	ctx := context.Background()
	storage, accounts, at := wakeStoreFixture(t, 1)
	account := accounts[0]
	if _, err := storage.db.Exec("UPDATE accounts SET quota=NULL WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.CreateWake(ctx, "manual-fence", WakeRequest{AccountID: account.ID}); err != nil {
		t.Fatal(err)
	}
	checkingAt := time.Now().UTC().Add(time.Millisecond)
	wakeStoreActivity(t, storage, checkingAt, 1)
	if _, err := storage.PrepareWake(ctx, checkingAt); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	jobs := wakeStoreJobs(t, storage)
	if len(jobs) != 1 || jobs[0].Phase != "checking" || jobs[0].QueryJobID == "" {
		t.Fatal(jobs)
	}
	queryAt := checkingAt.Add(time.Second)
	run, err := storage.ClaimAt(ctx, queryAt)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := wakeStoreQuota(t, storage, account, queryAt, core.WindowExpired, at.Add(-time.Second), at.Add(24*time.Hour))
	snapshot.ReadStartedAt = checkingAt.Add(-time.Hour)
	wakeStoreSnapshot(t, storage, account, snapshot)
	if _, err = storage.PrepareWake(ctx, queryAt.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal("old preflight read authorized send", err)
	}
	snapshot.ReadStartedAt = queryAt
	snapshot.ObservedAt = queryAt
	raw, _ := json.Marshal(snapshot)
	if err = storage.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	wakeStoreActivity(t, storage, queryAt.Add(2*time.Second), 1)
	prepared, err := storage.PrepareWake(ctx, queryAt.Add(2*time.Second))
	if err != nil || prepared.ID != jobs[0].ID {
		t.Fatal(prepared, err)
	}
}

func TestWakeDoesNotUseFullPlaceholderAsAutomaticCycleAndRejectsFieldStaleness(t *testing.T) {
	ctx := context.Background()
	storage, accounts, at := wakeStoreFixture(t, 1)
	account := accounts[0]
	snapshot := wakeStoreQuota(t, storage, account, at, core.WindowUnknown, at.Add(5*time.Hour), at.Add(7*24*time.Hour))
	config := policy.DefaultWake()
	config.Enabled, config.FiveHour, config.Weekly = true, true, true
	config.Delay = policy.Delay{}
	config.AllowedHours = []string{}
	wakeStoreEnable(t, storage, config)
	if count, err := storage.ScheduleWakes(ctx, at, "boot"); err != nil || count != 0 {
		t.Fatal("placeholder manufactured a reset intent", count, err)
	}
	if _, _, err := storage.CreateWake(ctx, "manual", WakeRequest{AccountID: account.ID}); err != nil {
		t.Fatal(err)
	}
	checkingAt := time.Now().UTC().Add(time.Millisecond)
	wakeStoreActivity(t, storage, checkingAt, 1)
	snapshot.Fields = map[string]quota.Stamp{"permission": {ObservedAt: at, ReadStartedAt: at}, "five_hour": {ObservedAt: at.Add(-time.Hour), ReadStartedAt: at.Add(-time.Hour)}, "weekly": {ObservedAt: at, ReadStartedAt: at}}
	wakeStoreSnapshot(t, storage, account, snapshot)
	if _, err := storage.PrepareWake(ctx, checkingAt); !errors.Is(err, ErrNotFound) {
		t.Fatal("fresh permission revived stale short field", err)
	}
	job := wakeStoreJobs(t, storage)[0]
	if job.Phase != "checking" || job.HoldReason != "quota_unknown" {
		t.Fatal(job)
	}
}

func TestWakeKnownUnsentBudgetCanRequeueButUnknownNeverCan(t *testing.T) {
	ctx := context.Background()
	storage, accounts, _ := wakeStoreFixture(t, 1)
	job, _, err := storage.CreateWake(ctx, "budget", WakeRequest{AccountID: accounts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Millisecond)
	prepared, err := storage.PrepareWake(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AuthorizeWake(ctx, prepared.ID, at); err != nil {
		t.Fatal(err)
	}
	if err = storage.FinishWake(ctx, job.ID, "queued", "model_busy", at); err != nil {
		t.Fatal(err)
	}
	current, _ := storage.Wake(ctx, job.ID)
	if current.Phase != "queued" || current.HoldReason != "model_busy" || current.Model != "" || current.HoldUntil != "" {
		t.Fatal(current)
	}
	prepared, err = storage.PrepareWake(ctx, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AuthorizeWake(ctx, prepared.ID, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = storage.FinishWake(ctx, job.ID, "unknown", "wake_outcome_unknown", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = storage.FinishWake(ctx, job.ID, "queued", "model_busy", at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	current, _ = storage.Wake(ctx, job.ID)
	if current.Phase != "unknown" {
		t.Fatal("unknown was requeued", current)
	}
}

func TestWakeCapacitySatisfiedEndsUnsentJobsAndRearmsOnDemand(t *testing.T) {
	for _, reason := range []string{"five_hour_reset", "daily"} {
		for _, phase := range []string{"queued", "checking", "prepared"} {
			t.Run(reason+"/"+phase, func(t *testing.T) {
				ctx := context.Background()
				storage, accounts, at := wakeStoreFixture(t, 2)
				// The other account has an active window but no usable permission yet.
				other := wakeStoreQuota(t, storage, accounts[1], at, core.WindowActive, at.Add(5*time.Hour), at.Add(24*time.Hour))
				blocked := core.PermissionBlocked
				other.Permission = &blocked
				wakeStoreSnapshot(t, storage, accounts[1], other)
				config := policy.DefaultWake()
				config.Enabled, config.FiveHour = true, reason == "five_hour_reset"
				config.DailyEnabled = reason == "daily"
				config.DailyTarget = 0
				if config.DailyEnabled {
					config.DailyTarget = 1
				}
				config.Timezone, config.DailyWindow = "UTC", "00:00-24:00"
				config.Delay, config.AllowedHours = policy.Delay{}, []string{}
				wakeStoreEnable(t, storage, config)
				want := 1
				if reason == "daily" {
					want = 2
				}
				if count, err := storage.ScheduleWakes(ctx, at, "boot"); err != nil || count != want {
					t.Fatal(count, err)
				}
				job := wakeStoreJobs(t, storage)[0]
				if reason == "daily" {
					// Daily scheduling also records a task for the other account.
					for _, candidate := range wakeStoreJobs(t, storage) {
						if candidate.AccountID == accounts[0].ID {
							job = candidate
						}
					}
				}
				if _, err := storage.db.Exec("UPDATE wake_jobs SET scheduled_at=CASE WHEN id=? THEN ? ELSE ? END", job.ID, queryStamp(at), queryStamp(at.Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
				if phase == "prepared" {
					if _, err := storage.PrepareWake(ctx, at); err != nil {
						t.Fatal(err)
					}
				} else if phase == "checking" {
					if _, err := storage.db.Exec("UPDATE accounts SET quota=NULL WHERE id=?", accounts[0].ID); err != nil {
						t.Fatal(err)
					}
					if _, err := storage.PrepareWake(ctx, at); !errors.Is(err, ErrNotFound) {
						t.Fatal(err)
					}
				}
				// Another account supplies the required capacity before the model is sent.
				settledAt := at.Add(2 * time.Second)
				wakeStoreActivity(t, storage, settledAt, 1)
				wakeStoreQuota(t, storage, accounts[1], settledAt, core.WindowActive, at.Add(5*time.Hour), at.Add(24*time.Hour))
				if phase == "prepared" {
					if _, err := storage.AuthorizeWake(ctx, job.ID, settledAt); !errors.Is(err, ErrNotFound) {
						t.Fatal("capacity changed before send", err)
					}
				} else if _, err := storage.PrepareWake(ctx, settledAt); !errors.Is(err, ErrNotFound) {
					t.Fatal("unnecessary model prepared", err)
				}
				skipped, err := storage.Wake(ctx, job.ID)
				if err != nil || skipped.Phase != "skipped" || skipped.HoldReason != "capacity_satisfied" || skipped.ErrorCode != "" || skipped.NextCheckAt != "" {
					t.Fatal("capacity hold did not terminate", skipped, err)
				}
				page := readHistory(t, storage, TaskHistoryFilter{Type: "wake", Search: job.ID})
				if page.Total != 1 || page.StatusCounts.Skipped != 1 || page.StatusCounts.Running != 0 || page.StatusCounts.Queued != 0 {
					t.Fatal("old task still displayed as active", page)
				}
				for tick := 3; tick <= 5; tick++ {
					if count, err := storage.ScheduleWakes(ctx, at.Add(time.Duration(tick)*time.Second), "boot"); err != nil || count != 0 {
						t.Fatal("capacity skip was recreated without demand", count, err)
					}
				}
				// Increased demand can reuse the same expired cycle, while the
				// historical skipped task remains terminal and unchanged.
				demandAt := at.Add(6 * time.Second)
				wakeStoreActivity(t, storage, demandAt, 2)
				wakeStoreQuota(t, storage, accounts[0], demandAt, core.WindowExpired, at.Add(-time.Second), at.Add(24*time.Hour))
				if count, err := storage.ScheduleWakes(ctx, demandAt, "boot"); err != nil || count != 1 {
					t.Fatal("same-cycle demand was lost", count, err)
				}
				if count, err := storage.ScheduleWakes(ctx, demandAt, "boot"); err != nil || count != 0 {
					t.Fatal("same-cycle demand duplicated", count, err)
				}
				if _, err := storage.db.Exec("UPDATE wake_jobs SET scheduled_at=? WHERE phase='queued'", queryStamp(demandAt)); err != nil {
					t.Fatal(err)
				}
				replacement, err := storage.PrepareWake(ctx, demandAt)
				if err != nil || replacement.ID == job.ID || replacement.Cycle != job.Cycle {
					t.Fatal("replacement not ready for demand", replacement, err)
				}
				if _, err = storage.AuthorizeWake(ctx, replacement.ID, demandAt); err != nil {
					t.Fatal(err)
				}
				old, err := storage.Wake(ctx, job.ID)
				if err != nil || old.Phase != "skipped" || old.UpdatedAt != skipped.UpdatedAt {
					t.Fatal("historical skip mutated", old, err)
				}
				if reason != "five_hour_reset" {
					return
				}
				if err = storage.FinishWake(ctx, replacement.ID, "verifying", "", demandAt); err != nil {
					t.Fatal(err)
				}
				proofAt := demandAt.Add(2 * time.Second)
				resetAt := proofAt.Add(5 * time.Hour)
				wakeStoreActivity(t, storage, proofAt, 2)
				wakeStoreQuota(t, storage, accounts[0], proofAt, core.WindowActive, resetAt, at.Add(24*time.Hour))
				if _, err = storage.ScheduleWakes(ctx, proofAt, "boot"); err != nil {
					t.Fatal(err)
				}
				verified, err := storage.Wake(ctx, replacement.ID)
				if err != nil || verified.Phase != "completed" {
					t.Fatal(verified, err)
				}
				// Advance over five hours with fresh inventory and activity, as a
				// continuously running daemon would observe them.
				nextAt := resetAt.Add(time.Second)
				if _, err = storage.db.Exec("UPDATE inventory_state SET last_success_at=?", queryStamp(nextAt)); err != nil {
					t.Fatal(err)
				}
				wakeStoreActivity(t, storage, nextAt, 2)
				wakeStoreQuota(t, storage, accounts[0], nextAt, core.WindowExpired, resetAt, at.Add(24*time.Hour))
				wakeStoreQuota(t, storage, accounts[1], nextAt, core.WindowActive, nextAt.Add(5*time.Hour), at.Add(24*time.Hour))
				// Both observed reset boundaries have passed; the other account's
				// already-active window will be skipped during its own preflight.
				if count, err := storage.ScheduleWakes(ctx, nextAt, "boot"); err != nil || count != 2 {
					t.Fatal("next reset cycle was lost", count, err)
				}
				next, err := storage.PrepareWake(ctx, nextAt)
				if err != nil || next.ID == replacement.ID || next.ID == job.ID || next.Cycle != queryStamp(resetAt) {
					t.Fatal("old task blocked next reset cycle", next, err)
				}
			})
		}
	}
}

func TestWakeCheckingReturnsToQueueWhenAdmissionIsHeld(t *testing.T) {
	ctx := context.Background()
	storage, accounts, _ := wakeStoreFixture(t, 1)
	job, _, err := storage.CreateWake(ctx, "checking-held", WakeRequest{AccountID: accounts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Millisecond)
	if _, err = storage.db.Exec("UPDATE accounts SET quota=NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.PrepareWake(ctx, at); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	checking, err := storage.Wake(ctx, job.ID)
	if err != nil || checking.Phase != "checking" {
		t.Fatal(checking, err)
	}
	if _, err = storage.db.Exec("UPDATE event_state SET connected=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.PrepareWake(ctx, at.Add(2*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	queued, err := storage.Wake(ctx, job.ID)
	if err != nil || queued.Phase != "queued" || queued.HoldReason != "activity_unknown" {
		t.Fatal("held preflight still displayed as executing", queued, err)
	}
}
