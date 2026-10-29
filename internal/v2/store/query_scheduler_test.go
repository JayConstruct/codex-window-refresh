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

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
)

func queryTestAccount(id string) Account {
	return Account{ID: id, AuthIndex: "index-" + id, Binding: "binding-" + id, Status: "available"}
}

func saveQueryTestSettings(t *testing.T, store *Store, config policy.QueryConfig) QuerySettings {
	t.Helper()
	current, err := store.QuerySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.SaveQuerySettings(context.Background(), current.Revision, config)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func createQueryTestJob(t *testing.T, store *Store, key, target string) Job {
	t.Helper()
	jobID, existing, err := store.CreateQuery(context.Background(), key, "hash-"+key, target)
	if err != nil || existing {
		t.Fatalf("CreateQuery(%q): existing=%v err=%v", key, existing, err)
	}
	job, err := store.Job(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func queryTestFreshInventory(t *testing.T, store *Store, at time.Time) {
	t.Helper()
	if _, err := store.db.Exec("UPDATE inventory_state SET last_success_at=? WHERE singleton=1", queryStamp(at)); err != nil {
		t.Fatal(err)
	}
}

func queryTestSchedule(t *testing.T, store *Store, at time.Time, want int) {
	t.Helper()
	created, err := store.ScheduleQueries(context.Background(), at)
	if err != nil || created != want {
		t.Fatalf("ScheduleQueries(%s) = %d, %v; want %d physical jobs", at.Format(time.RFC3339Nano), created, err, want)
	}
}

func queryTestNoClaim(t *testing.T, store *Store, at time.Time) {
	t.Helper()
	if run, err := store.ClaimAt(context.Background(), at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected dispatch at %s: run=%+v err=%v", at.Format(time.RFC3339Nano), run, err)
	}
}

func queryTestCount(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func queryTestScheduled(t *testing.T, store *Store, jobID, accountID string) time.Time {
	t.Helper()
	var stamp string
	if err := store.db.QueryRow("SELECT scheduled_at FROM query_schedule WHERE job_id=? AND account_id=?", jobID, accountID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("schedule is not UTC: %q", stamp)
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestQuerySchedulerDefaultsOffAndRequiresFreshBoundInventory(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	settings, err := store.QuerySettings(ctx)
	if err != nil || !reflect.DeepEqual(settings.Config, policy.DefaultQuery()) {
		t.Fatalf("defaults: %+v, %v", settings, err)
	}
	if version := queryTestCount(t, store, "SELECT version FROM schema_version"); version != 10 {
		t.Fatalf("schema version = %d, want 10", version)
	}
	account := queryTestAccount("account")
	if err = store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	queryTestSchedule(t, store, at, 0)
	config := settings.Config
	config.FiveHour = true
	config.Delay = policy.Delay{}
	saveQueryTestSettings(t, store, config)
	for _, status := range []string{"pending", "unavailable"} {
		if _, err = store.db.Exec("UPDATE inventory_state SET status=? WHERE singleton=1", status); err != nil {
			t.Fatal(err)
		}
		queryTestSchedule(t, store, at, 0)
	}
	if err = store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	queryTestFreshInventory(t, store, at.Add(-91*time.Second))
	queryTestSchedule(t, store, at, 0)
	account.Binding = ""
	if err = store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 0)
	account = queryTestAccount("account")
	if err = store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	queryTestFreshInventory(t, store, at.Add(-90*time.Second))
	queryTestSchedule(t, store, at, 1)
	var trigger string
	if err = store.db.QueryRow("SELECT trigger FROM query_plans").Scan(&trigger); err != nil || trigger != "initial_sync" {
		t.Fatalf("missing evidence did not schedule initial sync: %q, %v", trigger, err)
	}
	queryTestSchedule(t, store, at, 0)
}

func TestQuerySettingsConflictAndInvalidUpdateLeaveStateUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	initial, err := store.QuerySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := initial.Config
	config.Weekly = true
	saved := saveQueryTestSettings(t, store, config)
	if saved.Revision != initial.Revision+1 || saved.ViewRevision <= initial.ViewRevision {
		t.Fatalf("settings revisions did not advance: before=%+v after=%+v", initial, saved)
	}
	if _, err = store.SaveQuerySettings(ctx, initial.Revision, initial.Config); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("stale writer accepted: %v", err)
	}
	invalid := config
	invalid.Delay = policy.Delay{Min: 60, Max: 5}
	if _, err = store.SaveQuerySettings(ctx, saved.Revision, invalid); !errors.Is(err, policy.ErrInvalid) {
		t.Fatalf("invalid configuration accepted: %v", err)
	}
	current, err := store.QuerySettings(ctx)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("rejected update changed state: got=%+v want=%+v err=%v", current, saved, err)
	}
	unchanged, err := store.SaveQuerySettings(ctx, saved.Revision, saved.Config)
	if err != nil || !reflect.DeepEqual(unchanged, saved) {
		t.Fatalf("identical save changed revisions: %+v, %v", unchanged, err)
	}
}

func TestQueryBatchCapturesDelayAtAcceptanceAndSurvivesRestartAndReplay(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	config := policy.DefaultQuery()
	config.Delay = policy.Delay{Min: 40, Max: 40}
	saveQueryTestSettings(t, store, config)
	accounts := []Account{queryTestAccount("first"), queryTestAccount("second")}
	if err = store.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	job := createQueryTestJob(t, store, "batch", "")
	config.Delay = policy.Delay{Min: 120, Max: 120}
	saveQueryTestSettings(t, store, config)
	before := time.Now().UTC()
	if err = store.Expand(ctx, job, accounts); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	scheduled := queryTestScheduled(t, store, job.ID, accounts[0].ID)
	if scheduled.Before(before.Add(40*time.Second)) || scheduled.After(after.Add(40*time.Second)) {
		t.Fatalf("accepted delay not retained: due=%s expansion=[%s,%s]", scheduled, before, after)
	}
	if other := queryTestScheduled(t, store, job.ID, accounts[1].ID); !other.Equal(scheduled) {
		t.Fatalf("fixed per-item delay differs: %s, %s", scheduled, other)
	}
	queryTestNoClaim(t, store, scheduled.Add(-time.Nanosecond))
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	replayed, existing, err := store.CreateQuery(ctx, "batch", "hash-batch", "")
	if err != nil || !existing || replayed != job.ID {
		t.Fatalf("replay: id=%s existing=%v err=%v", replayed, existing, err)
	}
	if err = store.Expand(ctx, job, accounts); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if actual := queryTestScheduled(t, store, job.ID, account.ID); !actual.Equal(scheduled) {
			t.Fatalf("restart or replay redrew deadline: %s != %s", actual, scheduled)
		}
	}
	if _, _, err = store.CreateQuery(ctx, "batch", "different-hash", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting key replay accepted: %v", err)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_items"); count != 2 {
		t.Fatalf("replay expanded extra items: %d", count)
	}
	queryTestNoClaim(t, store, scheduled.Add(-time.Nanosecond))
	if _, err = store.ClaimAt(ctx, scheduled); err != nil {
		t.Fatalf("batch not claimable at saved deadline: %v", err)
	}
}

func TestQueryBatchUsesBoundedPerAccountUTCSchedules(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	accounts := make([]Account, 24)
	for index := range accounts {
		accounts[index] = queryTestAccount(fmt.Sprintf("account-%02d", index))
	}
	job := createQueryTestJob(t, store, "random-batch", "")
	before := time.Now().UTC()
	if err := store.Expand(ctx, job, accounts); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	for _, account := range accounts {
		scheduled := queryTestScheduled(t, store, job.ID, account.ID)
		if scheduled.Before(before.Add(5*time.Second)) || scheduled.After(after.Add(60*time.Second)) {
			t.Fatalf("%s scheduled outside [5s,60s]: %s", account.ID, scheduled)
		}
	}
	queryTestNoClaim(t, store, before.Add(5*time.Second-time.Nanosecond))
}

func TestQuerySinglePromotesBatchAndInflightDemandsShareOneRun(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	config := policy.DefaultQuery()
	config.Delay = policy.Delay{Min: 120, Max: 120}
	saveQueryTestSettings(t, store, config)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	batch := createQueryTestJob(t, store, "batch", "")
	if err := store.Expand(ctx, batch, []Account{account}); err != nil {
		t.Fatal(err)
	}
	batchDue := queryTestScheduled(t, store, batch.ID, account.ID)
	queryTestNoClaim(t, store, time.Now().UTC())
	single := createQueryTestJob(t, store, "single", account.ID)
	if err := store.Expand(ctx, single, []Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := store.Claim(ctx)
	if err != nil || run.Account.ID != account.ID {
		t.Fatalf("manual single did not immediately promote batch: %+v, %v", run, err)
	}
	inflight := createQueryTestJob(t, store, "inflight", account.ID)
	if err = store.Expand(ctx, inflight, []Account{account}); err != nil {
		t.Fatal(err)
	}
	queryTestNoClaim(t, store, batchDue.Add(time.Hour))
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_runs"); count != 1 {
		t.Fatalf("coalescing created %d physical runs", count)
	}
	if err = store.Finish(ctx, run, []byte(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []Job{batch, single, inflight} {
		job, err := store.Job(ctx, parent.ID)
		if err != nil || job.Phase != "completed" || len(job.Items) != 1 || job.Items[0].Phase != "completed" {
			t.Fatalf("coalesced parent not completed: %+v, %v", job, err)
		}
	}
	if actual := queryTestScheduled(t, store, batch.ID, account.ID); !actual.Equal(batchDue) {
		t.Fatalf("single promotion redrew batch schedule: %s != %s", actual, batchDue)
	}
}

func TestQueryExpiredWindowsCoalesceAndCompletedCyclesSurvivePruning(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	at := time.Now().UTC().Add(-time.Second)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.FiveHour, config.Weekly, config.Delay = true, true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	snapshot := dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	snapshot.FiveHour.State, snapshot.Weekly.State = core.WindowExpired, core.WindowExpired
	snapshot.FiveHour.ResetAt, snapshot.Weekly.ResetAt = at.Add(-time.Second), at.Add(-time.Minute)
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	if count := queryTestCount(t, store, "SELECT count(DISTINCT job_id) FROM query_timers WHERE kind IN ('five_hour_expiry','weekly_expiry')"); count != 1 {
		t.Fatalf("expiry demands did not coalesce: %d jobs", count)
	}
	queryTestSchedule(t, store, at, 0)
	run, err := store.ClaimAt(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	queryTestSchedule(t, store, at, 0)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_timers WHERE phase='completed'"); count != 2 {
		t.Fatalf("not all expiry timers completed: %d", count)
	}
	future := time.Now().UTC().Add(2 * time.Hour)
	if err = store.Prune(ctx, future, Retention{HistoryAge: time.Hour, TerminalJobs: 1}); err != nil {
		t.Fatal(err)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM jobs"); count != 0 {
		t.Fatalf("completed job not pruned: %d", count)
	}
	queryTestFreshInventory(t, store, future)
	queryTestSchedule(t, store, future, 0)
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_timers WHERE phase='completed'"); count != 2 {
		t.Fatalf("pruning erased cycle ledger: %d", count)
	}
}

func TestQueryUnknownFullPlaceholdersNeverBecomeExpiryBaselines(t *testing.T) {
	for _, resetOffset := range []time.Duration{-time.Minute, time.Minute} {
		t.Run(resetOffset.String(), func(t *testing.T) {
			ctx := context.Background()
			store := openTest(t)
			account := queryTestAccount("account")
			if err := store.SyncInventory(ctx, []Account{account}); err != nil {
				t.Fatal(err)
			}
			config := policy.DefaultQuery()
			config.FiveHour, config.Weekly, config.Delay = true, true, policy.Delay{}
			saveQueryTestSettings(t, store, config)
			at := time.Now().UTC().Add(-time.Second)
			snapshot := dashboardSnapshot(at)
			snapshot.Binding = account.Binding
			snapshot.FiveHour.State, snapshot.Weekly.State = core.WindowUnknown, core.WindowUnknown
			snapshot.FiveHour.Remaining.Value, snapshot.Weekly.Remaining.Value = 100, 100
			snapshot.FiveHour.ResetAt, snapshot.Weekly.ResetAt = at.Add(resetOffset), at.Add(resetOffset)
			saveDashboardSnapshot(t, store, account.ID, snapshot)
			queryTestFreshInventory(t, store, at)
			queryTestSchedule(t, store, at, 0)
			future := at.Add(2 * time.Minute)
			queryTestFreshInventory(t, store, future)
			queryTestSchedule(t, store, future, 0)
			if count := queryTestCount(t, store, "SELECT count(*) FROM query_baselines WHERE five_hour_reset!='' OR weekly_reset!=''"); count != 0 {
				t.Fatalf("unknown placeholders became baselines: %d", count)
			}
		})
	}
}

func TestQueryHistoricalActiveBoundarySurvivesUnknownEvidence(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.FiveHour, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC().Add(-time.Second)
	boundary := at.Add(time.Minute)
	snapshot := dashboardSnapshot(at)
	snapshot.Binding, snapshot.FiveHour.ResetAt = account.Binding, boundary
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 0)
	snapshot.FiveHour.State = core.WindowUnknown
	snapshot.FiveHour.ResetAt = at.Add(5 * time.Hour)
	snapshot.FiveHour.Remaining.Value = 100
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, boundary.Add(-time.Nanosecond))
	queryTestSchedule(t, store, boundary.Add(-time.Nanosecond), 0)
	queryTestFreshInventory(t, store, boundary)
	queryTestSchedule(t, store, boundary, 1)
	var cycle string
	if err := store.db.QueryRow("SELECT cycle FROM query_timers WHERE kind='five_hour_expiry'").Scan(&cycle); err != nil || cycle != queryStamp(boundary) {
		t.Fatalf("historical boundary lost: cycle=%q want=%q err=%v", cycle, queryStamp(boundary), err)
	}
}

func TestQueryDisableCancelsOnlyQueuedAutomaticAndReenableReschedules(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	accounts := []Account{queryTestAccount("first"), queryTestAccount("second"), queryTestAccount("manual")}
	accounts[2].Status = "disabled"
	if err := store.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.Automatic, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC()
	queryTestSchedule(t, store, at, 2)
	running, err := store.ClaimAt(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	config.Delay = policy.Delay{Min: 60, Max: 60}
	saveQueryTestSettings(t, store, config)
	manual := createQueryTestJob(t, store, "manual-batch", "")
	if err = store.Expand(ctx, manual, accounts[2:]); err != nil {
		t.Fatal(err)
	}
	manualDue := queryTestScheduled(t, store, manual.ID, accounts[2].ID)
	config.Automatic = false
	saveQueryTestSettings(t, store, config)
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_items WHERE phase='cancelled'"); count != 1 {
		t.Fatalf("disabled rule cancelled %d items, want only queued automatic", count)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_items WHERE phase='running'"); count != 1 {
		t.Fatalf("disabled rule disturbed running work: %d", count)
	}
	job, err := store.Job(ctx, manual.ID)
	if err != nil || job.Phase != "running" || job.Items[0].Phase != "queued" {
		t.Fatalf("manual work cancelled: %+v, %v", job, err)
	}
	queryTestSchedule(t, store, at, 0)
	config.Automatic, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	queryTestSchedule(t, store, at, 1)
	queryTestSchedule(t, store, at, 0)
	if actual := queryTestScheduled(t, store, manual.ID, accounts[2].ID); !actual.Equal(manualDue) {
		t.Fatalf("rule toggle redrew manual schedule: %s != %s", actual, manualDue)
	}
	if err = store.Finish(ctx, running, []byte(`{}`), ""); err != nil {
		t.Fatalf("disabled rule prevented inflight completion: %v", err)
	}
}

func TestQueryExclusionsApplyAtDispatchOnlyToAutomaticWithoutRedrawing(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	automatic, manual := queryTestAccount("automatic"), queryTestAccount("manual")
	manual.Status = "disabled"
	if err := store.SyncInventory(ctx, []Account{automatic, manual}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.Automatic, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Date(2030, 1, 1, 13, 59, 0, 0, time.UTC)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	var jobID string
	if err := store.db.QueryRow("SELECT id FROM jobs WHERE target=?", automatic.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	due := queryTestScheduled(t, store, jobID, automatic.ID)
	config.ExcludedHours = []string{"22:00-06:00"}
	saveQueryTestSettings(t, store, config)
	excluded := at.Add(time.Minute)
	queryTestNoClaim(t, store, excluded)
	queryTestFreshInventory(t, store, excluded)
	queryTestSchedule(t, store, excluded, 0)
	job := createTest(t, store, "manual-immediate", manual.ID)
	if err := store.Expand(ctx, job, []Account{manual}); err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimAt(ctx, excluded)
	if err != nil || run.Account.ID != manual.ID {
		t.Fatalf("exclusions delayed manual query: %+v, %v", run, err)
	}
	if err = store.Finish(ctx, run, []byte(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	queryTestNoClaim(t, store, time.Date(2030, 1, 1, 21, 59, 59, 0, time.UTC))
	run, err = store.ClaimAt(ctx, time.Date(2030, 1, 1, 22, 0, 0, 0, time.UTC))
	if err != nil || run.Account.ID != automatic.ID {
		t.Fatalf("automatic query not released at local 06:00: %+v, %v", run, err)
	}
	if actual := queryTestScheduled(t, store, jobID, automatic.ID); !actual.Equal(due) {
		t.Fatalf("exclusion redrew deadline: %s != %s", actual, due)
	}
}

func TestQueryBindingReplacementCannotDispatchOldDemandOrInheritExpiry(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.FiveHour, config.Weekly, config.Delay = true, true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC().Add(-time.Second)
	snapshot := dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	snapshot.FiveHour.ResetAt, snapshot.Weekly.ResetAt = at.Add(-time.Minute), at.Add(-time.Minute)
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	account.Binding = "replacement-binding"
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	snapshot = dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestNoClaim(t, store, at)
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_items WHERE phase='failed' AND error_code='account_binding_changed'"); count != 1 {
		t.Fatalf("replaced binding did not invalidate old demand: %d", count)
	}
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 0)
	queryTestNoClaim(t, store, at)
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_runs"); count != 0 {
		t.Fatalf("replacement triggered %d unauthorized physical queries", count)
	}
}

func TestQueryPeriodicIntervalStartsAtCompletionAndDoesNotAccumulate(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.Automatic, config.Interval, config.Delay = true, 60, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC().Add(-time.Second)
	snapshot := dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	later := at.Add(10 * time.Minute)
	queryTestFreshInventory(t, store, later)
	queryTestSchedule(t, store, later, 0)
	run, err := store.ClaimAt(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	queryTestSchedule(t, store, later, 0)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	var completedStamp string
	if err = store.db.QueryRow("SELECT updated_at FROM query_timers WHERE kind='periodic'").Scan(&completedStamp); err != nil {
		t.Fatal(err)
	}
	completed, err := time.Parse(time.RFC3339Nano, completedStamp)
	if err != nil {
		t.Fatal(err)
	}
	before := completed.Add(60*time.Second - time.Nanosecond)
	queryTestFreshInventory(t, store, before)
	queryTestSchedule(t, store, before, 0)
	due := completed.Add(60 * time.Second)
	queryTestFreshInventory(t, store, due)
	queryTestSchedule(t, store, due, 1)
	queryTestSchedule(t, store, due, 0)
	if count := queryTestCount(t, store, "SELECT count(*) FROM jobs"); count != 2 {
		t.Fatalf("periodic work accumulated: %d jobs", count)
	}
}

func TestQueryFailureRetriesUseExponentialBackoffCappedAtFifteenMinutes(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.FiveHour, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC().Add(-time.Second)
	snapshot := dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	snapshot.FiveHour.ResetAt = at.Add(-time.Minute)
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	for index, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		run, err := store.ClaimAt(ctx, at)
		if err != nil {
			t.Fatalf("attempt %d claim: %v", index+1, err)
		}
		if err = store.Finish(ctx, run, nil, "quota_transport_failed"); err != nil {
			t.Fatal(err)
		}
		var stamp string
		var attempts int
		if err = store.db.QueryRow("SELECT updated_at,attempts FROM query_timers WHERE kind='five_hour_expiry'").Scan(&stamp, &attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != index+1 {
			t.Fatalf("attempt ledger = %d, want %d", attempts, index+1)
		}
		failed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		before := failed.Add(delay - time.Nanosecond)
		queryTestFreshInventory(t, store, before)
		queryTestSchedule(t, store, before, 0)
		at = failed.Add(delay)
		queryTestFreshInventory(t, store, at)
		queryTestSchedule(t, store, at, 1)
		queryTestSchedule(t, store, at, 0)
	}
}

func TestQuerySchedulerScanCursorAndPhysicalJobCapDoNotStarveLaterAccounts(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	accounts := make([]Account, 330)
	for index := range accounts {
		accounts[index] = queryTestAccount(fmt.Sprintf("account-%03d", index))
	}
	if err := store.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.FiveHour, config.Delay = true, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC().Add(-time.Second)
	for _, account := range accounts[:256] {
		snapshot := dashboardSnapshot(at)
		snapshot.Binding = account.Binding
		saveDashboardSnapshot(t, store, account.ID, snapshot)
	}
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 0)
	queryTestSchedule(t, store, at, 64)
	queryTestSchedule(t, store, at, 10)
	queryTestSchedule(t, store, at, 0)
	if count := queryTestCount(t, store, "SELECT count(DISTINCT target) FROM jobs"); count != 74 {
		t.Fatalf("later accounts were starved or duplicated: %d", count)
	}
}

func TestQueryConcurrentClaimsShareOnePhysicalAccountRun(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	account := queryTestAccount("account")
	if err := store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first", "second"} {
		job := createQueryTestJob(t, store, key, account.ID)
		if err := store.Expand(ctx, job, []Account{account}); err != nil {
			t.Fatal(err)
		}
	}
	type claimResult struct {
		run Run
		err error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	at := time.Now().UTC()
	for range 2 {
		go func() {
			<-start
			run, err := store.ClaimAt(ctx, at)
			results <- claimResult{run: run, err: err}
		}()
	}
	close(start)
	claimed, empty := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.run.Account.ID == account.ID:
			claimed++
		case errors.Is(result.err, ErrNotFound):
			empty++
		default:
			t.Errorf("unexpected concurrent result: %+v", result)
		}
	}
	if claimed != 1 || empty != 1 {
		t.Fatalf("concurrent claims: claimed=%d empty=%d, want one each", claimed, empty)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_runs WHERE phase='running'"); count != 1 {
		t.Fatalf("concurrent claims created %d physical runs", count)
	}
	if count := queryTestCount(t, store, "SELECT count(DISTINCT run_id) FROM job_items WHERE phase='running'"); count != 1 {
		t.Fatalf("pending demands did not join one run: %d", count)
	}
}

func TestQueryAutomaticJobsDoNotConsumeManualReceiptBudgetAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			store.Close()
		}
	})
	account := queryTestAccount("account")
	if err = store.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultQuery()
	config.Automatic, config.Interval, config.Delay = true, 60, policy.Delay{}
	saveQueryTestSettings(t, store, config)
	at := time.Now().UTC()
	snapshot := dashboardSnapshot(at)
	snapshot.Binding = account.Binding
	saveDashboardSnapshot(t, store, account.ID, snapshot)
	queryTestFreshInventory(t, store, at)
	queryTestSchedule(t, store, at, 1)
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts"); count != 0 {
		t.Fatalf("automatic scheduling consumed %d manual receipts", count)
	}
	var automaticID string
	if err = store.db.QueryRow("SELECT job_id FROM query_timers WHERE kind='periodic'").Scan(&automaticID); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Job(ctx, automaticID)
	if err != nil || pending.Phase != "running" || len(pending.Items) != 1 || pending.Items[0].Phase != "queued" {
		t.Fatalf("automatic pending job: %+v, %v", pending, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts"); count != 0 {
		t.Fatalf("startup backfilled %d automatic receipts", count)
	}
	restored, err := store.Job(ctx, automaticID)
	if err != nil || restored.ID != pending.ID || restored.Phase != pending.Phase || !reflect.DeepEqual(restored.Items, pending.Items) {
		t.Fatalf("pending automatic job changed across restart: before=%+v after=%+v err=%v", pending, restored, err)
	}
	resumed := time.Now().UTC()
	queryTestFreshInventory(t, store, resumed)
	queryTestSchedule(t, store, resumed, 0)
	manual := createQueryTestJob(t, store, "manual-receipt", account.ID)
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts"); count != 1 {
		t.Fatalf("manual acceptance created %d receipts, want 1", count)
	}
	replayedID, existing, err := store.CreateQuery(ctx, "manual-receipt", "hash-manual-receipt", account.ID)
	if err != nil || !existing || replayedID != manual.ID {
		t.Fatalf("manual replay: id=%q existing=%v err=%v", replayedID, existing, err)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts"); count != 1 {
		t.Fatalf("manual replay changed receipt count to %d", count)
	}
	run, err := store.ClaimAt(ctx, resumed)
	if err != nil || run.Account.ID != account.ID {
		t.Fatalf("restored automatic job not claimable: %+v, %v", run, err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Job(ctx, automaticID)
	if err != nil || completed.Phase != "completed" {
		t.Fatalf("automatic completion: %+v, %v", completed, err)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, completed.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	due := completedAt.Add(time.Duration(config.Interval) * time.Second)
	queryTestFreshInventory(t, store, due)
	queryTestSchedule(t, store, due, 1)
	queryTestSchedule(t, store, due, 0)
	if count := queryTestCount(t, store, "SELECT count(*) FROM query_plans WHERE automatic=1"); count != 2 {
		t.Fatalf("next periodic automatic job missing or duplicated: %d", count)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts"); count != 1 {
		t.Fatalf("periodic completion or rescheduling consumed manual receipts: %d", count)
	}
	if count := queryTestCount(t, store, "SELECT count(*) FROM job_receipts WHERE job_id=?", manual.ID); count != 1 {
		t.Fatalf("manual receipt was replaced by an automatic receipt: %d", count)
	}
}

func TestQueryCoalescedBatchTimersFollowAccountCompletionNotParentOutcome(t *testing.T) {
	for _, refreshBeforeFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh_before_sibling_failure=%t", refreshBeforeFailure), func(t *testing.T) {
			ctx := context.Background()
			store := openTest(t)
			accounts := []Account{queryTestAccount("first"), queryTestAccount("second")}
			if err := store.SyncInventory(ctx, accounts); err != nil {
				t.Fatal(err)
			}
			config := policy.DefaultQuery()
			config.Interval, config.Delay = 60, policy.Delay{}
			saveQueryTestSettings(t, store, config)
			at := time.Now().UTC()
			for _, account := range accounts {
				snapshot := dashboardSnapshot(at)
				snapshot.Binding = account.Binding
				saveDashboardSnapshot(t, store, account.ID, snapshot)
			}
			batch := createQueryTestJob(t, store, "manual-batch", "")
			if err := store.Expand(ctx, batch, accounts); err != nil {
				t.Fatal(err)
			}
			siblingDue := at.Add(2 * time.Hour)
			config.Automatic = true
			saveQueryTestSettings(t, store, config)
			at = time.Now().UTC()
			queryTestFreshInventory(t, store, at)
			queryTestSchedule(t, store, at, 0)
			if count := queryTestCount(t, store, "SELECT count(*) FROM query_timers WHERE kind='periodic' AND job_id=?", batch.ID); count != 2 {
				t.Fatalf("periodic demands did not attach to manual batch: %d", count)
			}
			if _, err := store.db.Exec("UPDATE query_schedule SET scheduled_at=? WHERE job_id=? AND account_id=?", queryStamp(siblingDue), batch.ID, accounts[1].ID); err != nil {
				t.Fatal(err)
			}
			type timerState struct {
				jobID     string
				phase     string
				updatedAt string
			}
			readTimer := func(accountID string) timerState {
				t.Helper()
				var state timerState
				if err := store.db.QueryRow("SELECT job_id,phase,updated_at FROM query_timers WHERE account_id=? AND kind='periodic'", accountID).Scan(&state.jobID, &state.phase, &state.updatedAt); err != nil {
					t.Fatal(err)
				}
				return state
			}
			run, err := store.ClaimAt(ctx, at)
			if err != nil || run.Account.ID != accounts[0].ID {
				t.Fatalf("first account claim: %+v, %v", run, err)
			}
			snapshot := dashboardSnapshot(at)
			snapshot.Binding = accounts[0].Binding
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			beforeFinish := time.Now().UTC()
			if err = store.Finish(ctx, run, raw, ""); err != nil {
				t.Fatal(err)
			}
			afterFinish := time.Now().UTC()
			firstTimer, siblingTimer := readTimer(accounts[0].ID), readTimer(accounts[1].ID)
			if firstTimer.jobID != batch.ID || firstTimer.phase != "completed" || siblingTimer.jobID != batch.ID || siblingTimer.phase != "running" {
				t.Fatalf("per-account outcomes after first completion: first=%+v sibling=%+v", firstTimer, siblingTimer)
			}
			completedAt, err := time.Parse(time.RFC3339Nano, firstTimer.updatedAt)
			if err != nil || completedAt.Before(beforeFinish) || completedAt.After(afterFinish) {
				t.Fatalf("timer did not record actual account completion: %+v, err=%v", firstTimer, err)
			}
			parent, err := store.Job(ctx, batch.ID)
			if err != nil || parent.Phase != "running" || parent.Items[0].Phase != "completed" || parent.Items[1].Phase != "queued" {
				t.Fatalf("batch must remain running with sibling queued: %+v, %v", parent, err)
			}
			if refreshBeforeFailure {
				due := completedAt.Add(time.Duration(config.Interval) * time.Second)
				queryTestFreshInventory(t, store, due.Add(-time.Nanosecond))
				queryTestSchedule(t, store, due.Add(-time.Nanosecond), 0)
				queryTestFreshInventory(t, store, due)
				queryTestSchedule(t, store, due, 1)
				queryTestSchedule(t, store, due, 0)
				newTimer := readTimer(accounts[0].ID)
				if newTimer.jobID == batch.ID || newTimer.phase != "running" || readTimer(accounts[1].ID) != siblingTimer {
					t.Fatalf("periodic refresh blocked by pending sibling or disturbed its timer: first=%+v sibling=%+v", newTimer, readTimer(accounts[1].ID))
				}
				parent, err = store.Job(ctx, batch.ID)
				if err != nil || parent.Phase != "running" || parent.Items[1].Phase != "queued" {
					t.Fatalf("sibling did not remain queued during periodic refresh: %+v, %v", parent, err)
				}
				run, err = store.ClaimAt(ctx, due)
				if err != nil || run.Account.ID != accounts[0].ID {
					t.Fatalf("new periodic query not independently claimable: %+v, %v", run, err)
				}
				if err = store.Finish(ctx, run, raw, ""); err != nil {
					t.Fatal(err)
				}
				firstTimer = readTimer(accounts[0].ID)
				if firstTimer.jobID != newTimer.jobID || firstTimer.phase != "completed" {
					t.Fatalf("new periodic completion not committed: %+v", firstTimer)
				}
			}
			run, err = store.ClaimAt(ctx, siblingDue)
			if err != nil || run.Account.ID != accounts[1].ID {
				t.Fatalf("delayed sibling claim: %+v, %v", run, err)
			}
			if err = store.Finish(ctx, run, nil, "quota_transport_failed"); err != nil {
				t.Fatal(err)
			}
			if actual := readTimer(accounts[0].ID); actual != firstTimer {
				t.Fatalf("sibling failure rewrote successful account outcome or completion timestamp: before=%+v after=%+v", firstTimer, actual)
			}
			if actual := readTimer(accounts[1].ID); actual.jobID != batch.ID || actual.phase != "failed" {
				t.Fatalf("sibling failure not recorded independently: %+v", actual)
			}
			parent, err = store.Job(ctx, batch.ID)
			if err != nil || parent.Phase != "failed" || parent.Items[0].Phase != "completed" || parent.Items[1].Phase != "failed" {
				t.Fatalf("mixed batch outcome: %+v, %v", parent, err)
			}
		})
	}
}
