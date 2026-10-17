package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/quota"
)

func dashboardSnapshot(at time.Time) quota.Snapshot {
	permission := core.PermissionAllowed
	return quota.Snapshot{
		Binding: "binding", ObservedAt: at, ReadStartedAt: at,
		Permission: &permission,
		FiveHour:   &core.WindowEvidence{State: core.WindowActive, ResetAt: at.Add(time.Hour), Remaining: &core.QuotaAmount{Value: 60, Unit: "percent"}},
		Weekly:     &core.WindowEvidence{State: core.WindowActive, ResetAt: at.Add(24 * time.Hour), Remaining: &core.QuotaAmount{Value: 80, Unit: "percent"}},
	}
}

func saveDashboardSnapshot(t *testing.T, st *Store, id string, snapshot quota.Snapshot) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec("UPDATE accounts SET quota=?,observed_at=? WHERE id=?", string(raw), snapshot.ObservedAt.Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardCountsAllAccountsAndRequiresFreshActiveCapacity(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	accounts := []Account{}
	for index := range 620 {
		accounts = append(accounts, Account{ID: fmt.Sprintf("account-%03d", index), AuthIndex: "index", Status: "available"})
	}
	accounts[0].Status = "disabled"
	accounts[1].Status = "binding_unavailable"
	accounts[2].Status = "unavailable"
	if err := st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE accounts SET status='removed' WHERE id=?", accounts[3].ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Second)
	for index := range 12 {
		snapshot := dashboardSnapshot(at)
		switch index {
		case 4:
			snapshot.FiveHour.State = core.WindowUnknown
			snapshot.FiveHour.Remaining.Value = 100
		case 5:
			snapshot.FiveHour.ResetAt = at.Add(-time.Second)
		case 6:
			snapshot.ObservedAt = at.Add(-DashboardQuotaMaxAge - time.Second)
		case 7:
			snapshot.Fields = map[string]quota.Stamp{"permission": {ObservedAt: at.Add(-DashboardQuotaMaxAge - time.Second)}}
		case 8:
			snapshot.Fields = map[string]quota.Stamp{"five_hour": {ObservedAt: at.Add(-DashboardQuotaMaxAge - time.Second)}}
		case 9:
			snapshot.Weekly.Remaining.Value = 0
		case 10:
			*snapshot.Permission = core.PermissionBlocked
		}
		saveDashboardSnapshot(t, st, accounts[index].ID, snapshot)
	}
	dashboard, err := st.Dashboard(ctx)
	if err != nil || dashboard.Total != 619 || dashboard.Available != 616 || dashboard.Disabled != 1 || dashboard.Unavailable != 2 || dashboard.Capacity != 1 || dashboard.UnknownQuota != 613 || dashboard.Errors != 2 {
		t.Fatal(dashboard, err)
	}
	page, err := st.AccountPage(ctx, "", 6, nil)
	if err != nil || len(page.Accounts) != 6 || page.Total != dashboard.Total {
		t.Fatal(page, err)
	}
}

func TestDashboardJobsAndEventsAreDurableBoundedSummaries(t *testing.T) {
	dir := privateDir(t)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accounts := []Account{{ID: "a", AuthIndex: "a", Status: "available"}, {ID: "b", AuthIndex: "b", Status: "available"}}
	job := createTest(t, st, "batch", "")
	if err = st.Expand(ctx, job, accounts); err != nil {
		t.Fatal(err)
	}
	run, err := st.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := st.Dashboard(ctx)
	if err != nil || dashboard.Tasks.Active != 1 || dashboard.Tasks.Querying != 1 || dashboard.RecentJobs[0].Total != 2 {
		t.Fatal(dashboard, err)
	}
	if err = st.Finish(ctx, run, nil, "credential-that-must-not-be-exported"); err != nil {
		t.Fatal(err)
	}
	run, err = st.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.RecentJobs(ctx, 1)
	if err != nil || len(jobs) != 1 || jobs[0].Total != 2 || jobs[0].Failed != 1 || jobs[0].Completed != 1 {
		t.Fatal(jobs, err)
	}
	for index := range 25 {
		createTest(t, st, fmt.Sprintf("queued-%d", index), "a")
	}
	if err = st.InventoryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dashboard, err = st.Dashboard(ctx)
	if err != nil || dashboard.Tasks.Active != 25 || dashboard.Tasks.Querying != 0 || len(dashboard.RecentJobs) != DashboardRecentLimit || len(dashboard.RecentEvents) != DashboardRecentLimit || dashboard.Errors != 2 {
		t.Fatal(dashboard, err)
	}
	if dashboard.RecentEvents[0].Kind != "inventory" || dashboard.RecentEvents[0].At != "" {
		t.Fatal("invented inventory failure timestamp", dashboard.RecentEvents[0])
	}
	raw, _ := json.Marshal(dashboard)
	if strings.Contains(string(raw), "credential-that") || strings.Contains(string(raw), "\"items\"") {
		t.Fatal("summary exposed raw errors or unbounded items", string(raw))
	}
	if _, err = st.RecentJobs(ctx, 101); err == nil {
		t.Fatal("unbounded recent job list")
	}
}

func TestEmptyDashboardHasNoInventedEvents(t *testing.T) {
	dashboard, err := openTest(t).Dashboard(context.Background())
	if err != nil || len(dashboard.RecentJobs) != 0 || len(dashboard.RecentEvents) != 0 || dashboard.Total != 0 {
		t.Fatal(dashboard, err)
	}
}

func TestDashboardSharesOverviewSnapshotAcrossConcurrentInventoryUpdate(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.SyncInventory(ctx, []Account{{ID: "original", AuthIndex: "original", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	view, err := st.Overview(ctx)
	if err != nil || view.Revision != view.Dashboard.Revision || view.AccountsRevision != view.Dashboard.AccountsRevision || view.Dashboard.Total != 1 {
		t.Fatal(view, err)
	}
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var revision, accountsRevision uint64
	if err = tx.QueryRowContext(ctx, "SELECT revision,accounts_revision FROM view_state WHERE singleton=1").Scan(&revision, &accountsRevision); err != nil {
		t.Fatal(err)
	}
	if err = st.SyncInventory(ctx, []Account{{ID: "new-one", AuthIndex: "new-one", Status: "disabled"}, {ID: "new-two", AuthIndex: "new-two", Status: "disabled"}}); err != nil {
		t.Fatal(err)
	}
	dashboard, err := readDashboard(ctx, tx, revision, accountsRevision, view.Inventory)
	if err != nil || dashboard.Total != 1 || dashboard.Available != 1 || dashboard.Disabled != 0 || dashboard.Revision != view.Revision {
		t.Fatal("mixed transaction snapshot", dashboard, err)
	}
	updated, err := st.Overview(ctx)
	if err != nil || updated.Dashboard.Total != 2 || updated.Dashboard.Disabled != 2 || updated.Dashboard.Available != 0 || updated.Revision <= view.Revision || updated.Revision != updated.Dashboard.Revision || updated.AccountsRevision != updated.Dashboard.AccountsRevision {
		t.Fatal(updated, err)
	}
}

func TestDashboardActiveJobsKeepOldestWorkVisibleDespiteNewerHistory(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	active := []Job{}
	for index := range 25 {
		active = append(active, createTest(t, st, fmt.Sprintf("active-%d", index), "a"))
	}
	if err := st.Block(ctx, active[0].ID, "bridge_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := st.Expand(ctx, active[1], []Account{{ID: "a", AuthIndex: "a", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx); err != nil {
		t.Fatal(err)
	}
	for index := range 25 {
		completed := createTest(t, st, fmt.Sprintf("completed-%d", index), "")
		if err := st.Expand(ctx, completed, nil); err != nil {
			t.Fatal(err)
		}
	}
	dashboard, err := st.Dashboard(ctx)
	if err != nil || dashboard.Tasks.Active != 25 || dashboard.Tasks.Querying != 1 || len(dashboard.ActiveJobs) != 20 || len(dashboard.RecentJobs) != 20 {
		t.Fatal(dashboard, err)
	}
	for index, job := range dashboard.ActiveJobs {
		if job.ID != active[index].ID || job.Phase == "completed" {
			t.Fatal("active jobs were hidden or reordered", dashboard.ActiveJobs)
		}
	}
	if dashboard.ActiveJobs[0].ErrorCode != "bridge_unavailable" || dashboard.ActiveJobs[1].Phase != "running" {
		t.Fatal(dashboard.ActiveJobs)
	}
	for _, job := range dashboard.RecentJobs {
		if job.Phase != "completed" {
			t.Fatal("recent history order changed", dashboard.RecentJobs)
		}
	}
}

func TestJobSummaryErrorsAreAllowlistedAndListRevisionMatchesSnapshot(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	job := createTest(t, st, "failed-items", "")
	if err := st.Expand(ctx, job, []Account{{ID: "a", AuthIndex: "a", Status: "available"}, {ID: "b", AuthIndex: "b", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		run, err := st.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		code := "quota_authentication_failed"
		if run.Account.ID == "b" {
			code = "private-credential-details"
		}
		if err = st.Finish(ctx, run, nil, code); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.RecentJobsPage(ctx, 1)
	view, viewErr := st.Overview(ctx)
	if err != nil || viewErr != nil || len(page.Jobs) != 1 || page.Jobs[0].ErrorCode != "quota_authentication_failed" || page.Revision != view.Revision || view.Dashboard.RecentJobs[0].ErrorCode != "quota_authentication_failed" {
		t.Fatal(page, view, err, viewErr)
	}
	if _, err = st.db.Exec("UPDATE jobs SET error_code=? WHERE id=?", "private-job-error", job.ID); err != nil {
		t.Fatal(err)
	}
	page, err = st.RecentJobsPage(ctx, 1)
	if err != nil || page.Jobs[0].ErrorCode != "quota_query_failed" {
		t.Fatal(page, err)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("raw diagnostic escaped allowlist", string(raw))
	}
	missing := createTest(t, st, "missing-account", "missing")
	if err = st.Expand(ctx, missing, nil); err != nil {
		t.Fatal(err)
	}
	page, err = st.RecentJobsPage(ctx, 1)
	if err != nil || page.Jobs[0].ID != missing.ID || page.Jobs[0].ErrorCode != "account_not_found" {
		t.Fatal(page, err)
	}
}
