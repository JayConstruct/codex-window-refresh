package store

import (
	"codex-window-refresh/internal/core"
	"context"
	"errors"
	"testing"
	"time"
)

func TestEvidenceAccountFiltersAgreeWithCapacityAndPreservePagination(t *testing.T) {
	st, ctx := openTest(t), context.Background()
	accounts := []Account{{ID: "a", Status: "available"}, {ID: "b", Status: "available"}, {ID: "c", Status: "available"}, {ID: "d", Status: "available"}, {ID: "disabled", Status: "disabled"}, {ID: "unavailable", Status: "unavailable"}}
	if err := st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Second)
	for _, id := range []string{"a", "b", "c", "disabled"} {
		snapshot := dashboardSnapshot(at)
		if id == "c" {
			snapshot.Weekly.Remaining.Value = 0
		}
		saveDashboardSnapshot(t, st, id, snapshot)
	}
	first, err := st.AccountPageFiltered(ctx, "", 1, nil, "", "usable")
	if err != nil || first.Total != 6 || first.FilterTotal != 2 || first.Next != "a" || first.Accounts[0].ID != "a" {
		t.Fatal(first, err)
	}
	second, err := st.AccountPageFiltered(ctx, first.Next, 1, &first.AccountsRevision, "", "usable")
	if err != nil || second.Offset != 1 || second.Next != "" || len(second.Accounts) != 1 || second.Accounts[0].ID != "b" {
		t.Fatal(second, err)
	}
	unknown, err := st.AccountPageFiltered(ctx, "", 10, nil, "", "quota_unknown")
	if err != nil || unknown.FilterTotal != 2 || unknown.Accounts[0].ID != "d" || unknown.Accounts[1].ID != "unavailable" {
		t.Fatal(unknown, err)
	}
	snapshot := dashboardSnapshot(at.Add(-10 * time.Minute))
	saveDashboardSnapshot(t, st, "a", snapshot)
	if _, err = st.AccountPageFiltered(ctx, "a", 1, &first.AccountsRevision, "", "usable"); !errors.Is(err, ErrViewChanged) {
		t.Fatal("stale pagination accepted", err)
	}
	filtered, err := st.AccountPageFiltered(ctx, "", 10, nil, "UNAVAILABLE", "quota_unknown")
	if err != nil || filtered.FilterTotal != 1 {
		t.Fatal(filtered, err)
	}
	ordered, err := st.AccountPageSorted(ctx, "", 1, nil, "", "all", "weekly")
	if err != nil || ordered.Accounts[0].ID != "c" || ordered.Next != "c" {
		t.Fatal("global quota ordering did not precede pagination", ordered, err)
	}
	next, err := st.AccountPageSorted(ctx, ordered.Next, 1, &ordered.AccountsRevision, "", "all", "weekly")
	if err != nil || next.Offset != 1 || next.Accounts[0].ID == "c" {
		t.Fatal(next, err)
	}
}

func TestEvidenceFilterSetsMatchDashboardEnabledAndUnknownCounts(t *testing.T) {
	st, ctx := openTest(t), context.Background()
	accounts := []Account{{ID: "usable", Status: "available"}, {ID: "stale", Status: "available"}, {ID: "empty", Status: "available"}, {ID: "blocked", Status: "available"}, {ID: "disabled", Status: "disabled"}, {ID: "unavailable", Status: "unavailable"}, {ID: "binding", Status: "binding_unavailable"}}
	if err := st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Second)
	for _, a := range accounts {
		q := dashboardSnapshot(at)
		switch a.ID {
		case "stale":
			q = dashboardSnapshot(at.Add(-10 * time.Minute))
		case "empty":
			q.FiveHour.Remaining.Value = 0
			q.Weekly.Remaining.Value = 0
		case "blocked":
			blocked := core.PermissionBlocked
			q.Permission = &blocked
		}
		saveDashboardSnapshot(t, st, a.ID, q)
	}
	view, err := st.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := st.AccountPageFiltered(ctx, "", 100, nil, "", "quota_unknown")
	if err != nil {
		t.Fatal(err)
	}
	usable, err := st.AccountPageFiltered(ctx, "", 100, nil, "", "usable")
	if err != nil {
		t.Fatal(err)
	}
	if view.Dashboard.Enabled != 6 || view.Dashboard.UnknownEnabled != 3 || unknown.FilterTotal != view.Dashboard.UnknownEnabled || usable.FilterTotal != view.Dashboard.Capacity || view.Dashboard.WeeklyExhausted != 1 || view.Dashboard.FiveHourExhausted != 0 {
		t.Fatalf("dashboard %+v unknown %+v usable %+v", view.Dashboard, unknown, usable)
	}
}
