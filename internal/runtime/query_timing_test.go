package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func TestPeriodicQueriesUseIndependentParticipationExclusionsAndDelay(t *testing.T) {
	engine, host, _, clock := fixture(t, false, false)
	engine.cfg.Enabled = false
	engine.cfg.QuotaQuery.FiveHour.PeriodicEnabled = false
	engine.cfg.QuotaQuery.Weekly.Periodic.ExcludedHours = []string{"00:00-24:00"}
	step := func() {
		t.Helper()
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if host.reads != 0 {
		t.Fatal("disabled or excluded periodic policy queried")
	}
	engine.cfg.QuotaQuery.FiveHour.PeriodicEnabled = true
	engine.cfg.QuotaQuery.FiveHour.Interval = config.Duration(time.Minute)
	engine.cfg.QuotaQuery.FiveHour.Periodic.RandomDelay = config.DelayRange{Min: config.Duration(10 * time.Second), Max: config.Duration(10 * time.Second)}
	clock.now = clock.now.Add(9 * time.Second)
	step()
	if host.reads != 0 {
		t.Fatal("initial periodic random delay ignored")
	}
	clock.now = clock.now.Add(time.Second)
	step()
	if host.reads != 1 {
		t.Fatal("eligible five-hour policy blocked by weekly exclusions")
	}
	clock.now = clock.now.Add(69 * time.Second)
	step()
	if host.reads != 1 {
		t.Fatal("per-window interval or random delay ignored")
	}
	clock.now = clock.now.Add(time.Second)
	step()
	if host.reads != 2 || len(host.sends) != 0 {
		t.Fatal("periodic deadline did not run as query-only")
	}
}

func TestExpiryExclusionsAndDelayMatchPlanAndDoNotMarkBlockedWindow(t *testing.T) {
	engine, host, _, clock := fixture(t, false, true)
	engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
	engine.cfg.Planning.Enabled = true
	engine.cfg.QuotaQuery.OnFiveHourExpiry, engine.cfg.QuotaQuery.OnWeeklyExpiry = true, true
	engine.cfg.QuotaQuery.FiveHour.Expiry.ExcludedHours = []string{"10:00-11:00"}
	engine.cfg.QuotaQuery.FiveHour.Expiry.RandomDelay = config.DelayRange{Min: config.Duration(5 * time.Minute), Max: config.Duration(5 * time.Minute)}
	engine.cfg.QuotaQuery.Weekly.Expiry.RandomDelay = config.DelayRange{Min: config.Duration(10 * time.Minute), Max: config.Duration(10 * time.Minute)}
	account := engine.state.Accounts["a"]
	reset := clock.now
	account.FiveHourBaseline.StableResetAt, account.WeeklyBaseline.StableResetAt = reset, reset
	engine.state.Accounts["a"] = account
	engine.host = controlledHost{Host: host, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		host.reads++
		permission := core.PermissionAllowed
		observation := core.QuotaObservation{AccountID: read.AccountID, Generation: read.Generation, ReceivedAt: clock.now, Permission: &permission, FiveHour: &core.WindowEvidence{State: core.WindowUnknown, ResetAt: reset}, Weekly: &core.WindowEvidence{State: core.WindowExpired, ResetAt: reset}}
		if host.reads > 1 {
			observation.FiveHour.State, observation.Weekly = core.WindowExpired, nil
		}
		return observation, nil
	}}
	plan := engine.schedulePlan(engine.state, clock.now)
	if len(plan.Entries) != 2 {
		t.Fatalf("missing independent expiry plans: %+v", plan)
	}
	for _, entry := range plan.Entries {
		expected := reset.Add(10 * time.Minute)
		if entry.Trigger == string(core.ReasonFiveHour) {
			expected = reset.Add(time.Hour)
		}
		if !entry.EarliestAt.Equal(expected) {
			t.Fatalf("plan ignored exclusion or delay: %+v", entry)
		}
	}
	clock.now = reset.Add(9 * time.Minute)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 0 {
		t.Fatal("expiry delay ignored")
	}
	clock.now = reset.Add(10 * time.Minute)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	account = engine.state.Accounts["a"]
	if host.reads != 1 || !account.FiveHourExpiryQueried.IsZero() || !account.WeeklyExpiryQueried.Equal(reset) {
		t.Fatal("blocked five-hour expiry incorrectly completed by weekly query")
	}
	clock.now = reset.Add(time.Hour)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 2 || !engine.state.Accounts["a"].FiveHourExpiryQueried.Equal(reset) {
		t.Fatalf("excluded expiry failed to resume at interval end: reads=%d account=%+v", host.reads, engine.state.Accounts["a"])
	}
}

func TestManualAndVerificationReadsBypassQueryExclusions(t *testing.T) {
	for _, verification := range []bool{false, true} {
		engine, host, _, clock := fixture(t, false, false)
		engine.cfg.Enabled = false
		engine.cfg.QuotaQuery.FiveHour.Periodic.ExcludedHours = []string{"00:00-24:00"}
		engine.cfg.QuotaQuery.Weekly.Periodic.ExcludedHours = []string{"00:00-24:00"}
		if verification {
			account := engine.state.Accounts["a"]
			account.Task = &core.MergedTask{AccountID: "a", Generation: account.Generation, OperationID: "pending", State: core.TaskVerifying, NextVerificationAt: clock.now}
			engine.state.Accounts["a"] = account
		} else {
			engine.RequestQuotaQuery()
		}
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.reads != 1 || len(host.sends) != 0 {
			t.Fatalf("excluded automatic policy blocked explicit read or verification: %t", verification)
		}
	}
}

func TestQueryExclusionsRespectTimezoneCrossMidnightAndDST(t *testing.T) {
	settings := config.Defaults()
	for _, scenario := range []struct{ zone, from, excluded, expected string }{
		{"Asia/Shanghai", "2026-10-13T23:00:00+08:00", "22:00-08:00", "2026-10-14T08:00:00+08:00"},
		{"Asia/Shanghai", "2026-10-13T08:00:00+08:00", "22:00-08:00", "2026-10-13T08:00:00+08:00"},
		{"America/New_York", "2026-03-08T01:30:00-05:00", "01:00-03:00", "2026-03-08T03:00:00-04:00"},
		{"America/New_York", "2026-11-01T01:50:00-04:00", "01:40-02:00", "2026-11-01T01:00:00-05:00"},
	} {
		settings.Timezone = scenario.zone
		from, _ := time.Parse(time.RFC3339, scenario.from)
		expected, _ := time.Parse(time.RFC3339, scenario.expected)
		actual, ok := nextQueryTime(settings, []string{scenario.excluded}, from, from.Add(48*time.Hour))
		if !ok || !actual.Equal(expected) {
			t.Fatalf("wrong query exclusion deadline: %+v => %v", scenario, actual)
		}
	}
	from := time.Now()
	if _, ok := nextQueryTime(settings, []string{"00:00-24:00"}, from, from.Add(48*time.Hour)); ok {
		t.Fatal("all-day exclusion scheduled a query")
	}
}

func TestQueryRandomDelayIsStableAndBounded(t *testing.T) {
	account := core.AccountState{ID: "a", Generation: "generation"}
	anchor := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	delay := config.DelayRange{Min: config.Duration(time.Minute), Max: config.Duration(5 * time.Minute)}
	for _, weekly := range []bool{false, true} {
		for _, periodic := range []bool{false, true} {
			first := queryDelay(account, weekly, periodic, anchor, delay)
			if first < time.Minute || first > 5*time.Minute || first%time.Second != 0 || first != queryDelay(account, weekly, periodic, anchor, delay) {
				t.Fatal("query preview resampled or exceeded random delay bounds")
			}
		}
	}
}

func TestPeriodicQueryUsesOneDecisionInstant(t *testing.T) {
	engine, _, _, clock := fixture(t, false, false)
	now := clock.now
	engine.clock = &testClock{now.Add(time.Second)}
	if !engine.periodicQueryDue(engine.state.Accounts["a"], now) {
		t.Fatal("periodic eligibility advanced its clock beyond the supplied decision instant")
	}
}

func TestPlanShowsAllDayExclusionsButOmitsDeadlinesBeyondHorizon(t *testing.T) {
	engine, _, _, clock := fixture(t, false, false)
	engine.cfg.Enabled = false
	engine.cfg.Planning.Enabled = true
	engine.cfg.Planning.HorizonDays = 1
	engine.cfg.QuotaQuery.FiveHour.Periodic.ExcludedHours = []string{"00:00-24:00"}
	engine.cfg.QuotaQuery.Weekly.PeriodicEnabled = false
	plan := engine.schedulePlan(engine.state, clock.now)
	if len(plan.Entries) != 1 || plan.Entries[0].Status != "blocked" {
		t.Fatal("all-day exclusion was not explained in plan")
	}
	engine.cfg.QuotaQuery.FiveHour.Periodic.ExcludedHours = nil
	engine.cfg.QuotaQuery.FiveHour.Periodic.RandomDelay = config.DelayRange{Min: config.Duration(24 * time.Hour), Max: config.Duration(24 * time.Hour)}
	engine.nextRead["a"] = clock.now.Add(time.Hour)
	if plan := engine.schedulePlan(engine.state, clock.now); len(plan.Entries) != 0 {
		t.Fatal("out-of-horizon deadline displayed as a blocked task")
	}
}
