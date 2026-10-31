package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/quota"
)

const activeQueryEvidence = `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"window_minutes":300,"reset_after_seconds":3000},"secondary_window":{"used_percent":30,"window_minutes":10080,"reset_after_seconds":30000}}}`

// A completed quota read proves that an observation was saved. It does not
// prove that every window was reported or that a zero-usage cycle is active.
func TestCompletedQuotaQueryKeepsFreshUnknownAndPartialEvidenceOutOfCapacity(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		five, weekly   core.WindowState
	}{
		{
			name:     "full zero usage placeholders",
			response: `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"window_minutes":300,"reset_after_seconds":18000},"secondary_window":{"used_percent":0,"window_minutes":10080,"reset_after_seconds":604800}}}`,
			five:     core.WindowUnknown,
			weekly:   core.WindowUnknown,
		},
		{
			name:     "active short window and unknown zero usage weekly window",
			response: `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"window_minutes":300,"reset_after_seconds":3000},"secondary_window":{"used_percent":0,"window_minutes":10080,"reset_after_seconds":30000}}}`,
			five:     core.WindowActive,
			weekly:   core.WindowUnknown,
		},
		{
			name:     "five hour window without weekly window",
			response: `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"window_minutes":300,"reset_after_seconds":3000}}}`,
			five:     core.WindowActive,
		},
		{
			name:     "weekly window in primary slot without five hour window",
			response: `{"plan_type":"free","rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"window_minutes":10080,"reset_after_seconds":30000}}}`,
			weekly:   core.WindowActive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			a := Account{ID: "quota-account", AuthIndex: "quota-index", Binding: "quota-binding", Status: "available"}
			at := time.Now().UTC().Add(-10 * time.Second)
			observed := finishQuotaFreshnessQuery(t, s, a, "partial", tc.response, at)
			for _, window := range []struct {
				name  string
				value *core.WindowEvidence
				state core.WindowState
			}{{"five_hour", observed.FiveHour, tc.five}, {"weekly", observed.Weekly, tc.weekly}} {
				if window.state == "" {
					if window.value != nil {
						t.Fatalf("missing %s window was manufactured: %+v", window.name, window.value)
					}
					if _, exists := observed.Fields[window.name]; exists {
						t.Fatalf("missing %s gained a freshness stamp", window.name)
					}
					continue
				}
				if window.value == nil || window.value.State != window.state || !observed.Fields[window.name].ObservedAt.Equal(at) {
					t.Fatalf("fresh %s evidence lost: %+v; stamp=%+v", window.name, window.value, observed.Fields[window.name])
				}
			}
			assertQuotaFreshnessCapacity(t, s, 0, 1)

			// A later complete, active observation restores capacity through the
			// same query commit path, without changing the account identity.
			active := finishQuotaFreshnessQuery(t, s, a, "active", activeQueryEvidence, at.Add(time.Second))
			if active.FiveHour.State != core.WindowActive || active.Weekly.State != core.WindowActive {
				t.Fatal("active recovery observation lost", active)
			}
			assertQuotaFreshnessCapacity(t, s, 1, 0)
		})
	}
}

func TestCompletedPartialQuotaQueryPreservesOldWeeklyTimestampUntilReported(t *testing.T) {
	s := openTest(t)
	a := Account{ID: "quota-account", AuthIndex: "quota-index", Binding: "quota-binding", Status: "available"}
	at := time.Now().UTC().Add(-10 * time.Second)
	oldAt := at.Add(-10 * time.Minute)
	old := finishQuotaFreshnessQuery(t, s, a, "old", activeQueryEvidence, oldAt)
	assertQuotaFreshnessCapacity(t, s, 0, 1)

	partial := `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":40,"window_minutes":300,"reset_after_seconds":2900}}}`
	for index, stamp := range []time.Time{at, at.Add(time.Second)} {
		key := []string{"fresh-partial", "repeat-partial"}[index]
		observed := finishQuotaFreshnessQuery(t, s, a, key, partial, stamp)
		if !observed.Fields["permission"].ObservedAt.Equal(stamp) || !observed.Fields["five_hour"].ObservedAt.Equal(stamp) {
			t.Fatal("reported fields were not refreshed", observed.Fields)
		}
		if observed.Weekly == nil || observed.Weekly.Remaining.Value != old.Weekly.Remaining.Value || !observed.Fields["weekly"].ObservedAt.Equal(oldAt) {
			t.Fatal("omitted weekly field was erased or given false freshness", observed)
		}
		assertQuotaFreshnessCapacity(t, s, 0, 1)
	}

	finishQuotaFreshnessQuery(t, s, a, "full-recovery", activeQueryEvidence, at.Add(2*time.Second))
	assertQuotaFreshnessCapacity(t, s, 1, 0)
}

func TestQuotaCapacityExpiryDoesNotChangeAccountStatusOrStoredEvidence(t *testing.T) {
	s := openTest(t)
	a := Account{ID: "quota-account", AuthIndex: "quota-index", Binding: "quota-binding", Status: "available"}
	at := time.Now().UTC().Add(-DashboardQuotaMaxAge - time.Second)
	observed := finishQuotaFreshnessQuery(t, s, a, "active-before-expiry", activeQueryEvidence, at)
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	boundary := at.Add(DashboardQuotaMaxAge)
	if known, usable := quotaState(raw, boundary); !known || !usable {
		t.Fatal("complete active quota expired before the freshness boundary")
	}
	if known, usable := quotaState(raw, boundary.Add(time.Nanosecond)); known || usable {
		t.Fatal("capacity accepted evidence older than the freshness limit")
	}

	ctx := context.Background()
	before, err := s.AccountPage(ctx, "", 10, nil)
	if err != nil || len(before.Accounts) != 1 {
		t.Fatal(before, err)
	}
	assertQuotaFreshnessCapacity(t, s, 0, 1)
	dashboard, err := s.Dashboard(ctx)
	if err != nil || dashboard.Normal != 1 || dashboard.Available != 1 || dashboard.Enabled != 1 {
		t.Fatalf("quota freshness changed CPA account health: %+v err=%v", dashboard, err)
	}
	after, err := s.AccountPageFiltered(ctx, "", 10, nil, "", "available")
	if err != nil || after.FilterTotal != 1 || len(after.Accounts) != 1 {
		t.Fatal("account vanished from CPA available filter", after, err)
	}
	account := after.Accounts[0]
	if account.Status != "available" || account.DisplayStatus != "available" || account.ObservedAt != before.Accounts[0].ObservedAt || !bytes.Equal(account.Quota, before.Accounts[0].Quota) {
		t.Fatalf("reading stale quota altered account status or stored observation: before=%+v after=%+v", before.Accounts[0], account)
	}
}

func finishQuotaFreshnessQuery(t *testing.T, s *Store, account Account, key, response string, at time.Time) quota.Snapshot {
	t.Helper()
	ctx := context.Background()
	if err := s.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	job := createTest(t, s, key, account.ID)
	if err := s.Expand(ctx, job, []Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := quota.Normalize([]byte(response), nil, account.Binding, "cpa_wham_usage", at, at.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	completed, err := s.Job(ctx, job.ID)
	if err != nil || completed.Phase != "completed" || len(completed.Items) != 1 || completed.Items[0].Phase != "completed" || completed.Items[0].ErrorCode != "" {
		t.Fatalf("successful query did not complete: %+v, err=%v", completed, err)
	}
	page, err := s.AccountPage(ctx, "", 10, nil)
	if err != nil || len(page.Accounts) != 1 {
		t.Fatal(page, err)
	}
	var saved quota.Snapshot
	if err = json.Unmarshal(page.Accounts[0].Quota, &saved); err != nil {
		t.Fatal(err)
	}
	accountAt, err := time.Parse(time.RFC3339Nano, page.Accounts[0].ObservedAt)
	if err != nil || !accountAt.Equal(at) || !saved.ObservedAt.Equal(at) {
		t.Fatalf("query timestamp did not reach account API: account=%s snapshot=%s want=%s err=%v", page.Accounts[0].ObservedAt, saved.ObservedAt, at, err)
	}
	return saved
}

func assertQuotaFreshnessCapacity(t *testing.T, s *Store, capacity, unknown int) {
	t.Helper()
	ctx := context.Background()
	dashboard, err := s.Dashboard(ctx)
	if err != nil || dashboard.Total != 1 || dashboard.Capacity != capacity || dashboard.UnknownEnabled != unknown || dashboard.UnknownQuota != unknown {
		t.Fatalf("dashboard capacity=%d unknown=%d: %+v err=%v", capacity, unknown, dashboard, err)
	}
	for filter, count := range map[string]int{"usable": capacity, "quota_unknown": unknown} {
		page, err := s.AccountPageFiltered(ctx, "", 10, nil, "", filter)
		if err != nil || page.FilterTotal != count || len(page.Accounts) != count {
			t.Fatalf("%s filter disagrees with dashboard: %+v err=%v", filter, page, err)
		}
	}
}
