package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func TestOverdueWakeKeepsDeadlineSeparateFromMovingChecks(t *testing.T) {
	for _, reason := range []core.Reason{core.ReasonFiveHour, core.ReasonWeekly, core.ReasonDaily} {
		t.Run(string(reason), func(t *testing.T) {
			e, _, _, clock := fixture(t, false, reason == core.ReasonWeekly)
			due := clock.now.Add(-2 * time.Minute)
			a := e.state.Accounts["a"]
			a.FiveHourBaseline = core.WindowBaseline{CycleID: "expired", StableResetAt: due}
			a.WeeklyBaseline = core.WindowBaseline{}
			if reason == core.ReasonWeekly {
				a.FiveHourBaseline = core.WindowBaseline{}
				a.WeeklyBaseline = core.WindowBaseline{CycleID: "expired", StableResetAt: due}
			}
			if reason == core.ReasonDaily {
				a.FiveHourBaseline = core.WindowBaseline{}
				e.cfg.DailyRules = []config.DailyRule{{ID: "point", Enabled: true, RandomTimeRange: "09:00-09:00"}}
			}
			a.Task = &core.MergedTask{State: core.TaskWaiting, HoldReason: core.HoldQuotaUnknown, Intents: []core.Intent{{ID: "stored", Reason: reason, RuleID: "point", OriginalDueAt: due, RandomizedDueAt: due, Status: core.IntentPending}}}
			e.state.Accounts["a"] = a
			for index := 0; index < 3; index++ {
				e.nextPreflight["a"] = clock.now.Add(30 * time.Second)
				wake := e.schedulePlan(e.state, clock.now).NextWakes["a"]
				if wake.ScheduledAt == nil || !wake.ScheduledAt.Equal(due) || wake.Status != "waiting" || wake.WaitReason != core.HoldQuotaUnknown || wake.NextCheckAt == nil || !wake.NextCheckAt.Equal(e.nextPreflight["a"]) {
					t.Fatalf("retry replaced persisted deadline or lost wait reason: %+v", wake)
				}
				clock.now = clock.now.Add(time.Minute)
			}
		})
	}
}

func TestExpiredFiveHourWakeWaitsForDemandWithoutResampling(t *testing.T) {
	e, host, _, clock := fixture(t, false, false)
	e.cfg.QuotaQuery.Automatic = false
	var deadline time.Time
	for index := 0; index < 3; index++ {
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		wake := e.SchedulerDiagnostics().(SchedulingSummary).Plan.NextWakes["a"]
		if wake.ScheduledAt == nil || wake.Status != "waiting" || wake.WaitReason != core.HoldNoCapacity {
			t.Fatalf("missing demand wait explanation: %+v", wake)
		}
		if deadline.IsZero() {
			deadline = *wake.ScheduledAt
		} else if !wake.ScheduledAt.Equal(deadline) {
			t.Fatal("waiting for users postponed the persisted wake")
		}
		clock.now = clock.now.Add(time.Minute)
	}
	if host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("showing a waiting plan performed upstream work")
	}
	addUser(e, clock)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 {
		t.Fatal("persisted overdue wake failed to run when demand arrived")
	}
}

func TestFuturePlanHasIndependentQueriesAndWakesWithoutChangingState(t *testing.T) {
	engine, host, _, clock := fixture(t, false, true)
	engine.cfg.Planning.Enabled = true
	engine.cfg.Planning.HorizonDays = 2
	engine.cfg.QuotaQuery.Automatic = false
	engine.cfg.QuotaQuery.OnFiveHourExpiry, engine.cfg.QuotaQuery.OnWeeklyExpiry = true, true
	account := engine.state.Accounts["a"]
	account.FiveHourBaseline.StableResetAt = clock.now.Add(time.Hour)
	account.WeeklyBaseline.StableResetAt = clock.now.Add(24 * time.Hour)
	engine.state.Accounts["a"] = account
	before, _ := json.Marshal(engine.state)
	plan := engine.schedulePlan(engine.state, clock.now)
	if !plan.Enabled || len(plan.Entries) != 4 {
		t.Fatalf("expected independent queries and wakes: %+v", plan)
	}
	after, _ := json.Marshal(engine.state)
	if string(before) != string(after) || host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("planning changed persisted intent randomness or dispatched requests")
	}
	engine.cfg.Enabled = false
	plan = engine.schedulePlan(engine.state, clock.now)
	if len(plan.Entries) != 2 {
		t.Fatal("automatic wake off removed query plans or added wake plans")
	}
	engine.cfg.Planning.Enabled = false
	plan = engine.schedulePlan(engine.state, clock.now)
	if plan.Enabled || len(plan.Entries) != 0 {
		t.Fatal("planning off retained generated entries")
	}
}

func TestUnknownWindowsDoNotCreateFutureExpiryCycles(t *testing.T) {
	engine, _, _, clock := fixture(t, false, true)
	engine.cfg.Planning.Enabled = true
	engine.cfg.QuotaQuery.Automatic = false
	engine.cfg.QuotaQuery.OnFiveHourExpiry, engine.cfg.QuotaQuery.OnWeeklyExpiry = true, true
	account := engine.state.Accounts["a"]
	account.FiveHourBaseline, account.WeeklyBaseline = core.WindowBaseline{}, core.WindowBaseline{}
	account.FiveHour = core.WindowEvidence{State: core.WindowUnknown, ResetAt: clock.now.Add(5 * time.Hour), Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}}
	engine.state.Accounts["a"] = account
	if plan := engine.schedulePlan(engine.state, clock.now); len(plan.Entries) != 0 {
		t.Fatal("query placeholders were used to invent future cycles")
	}
}

func TestPlanUsesPersistedDeadlinesAndDailyPoints(t *testing.T) {
	engine, _, _, clock := fixture(t, false, false)
	engine.cfg.Planning.Enabled = true
	engine.cfg.Planning.HorizonDays = 2
	engine.cfg.QuotaQuery.Automatic = false
	engine.cfg.DailyRules = []config.DailyRule{{ID: "point", Enabled: true, RandomTimeRange: "11:00-11:00", MinimumReadyAccounts: 1, CatchUp: true}}
	account := engine.state.Accounts["a"]
	reset := clock.now.Add(time.Hour)
	account.FiveHourBaseline.StableResetAt = reset
	due := reset.Add(2 * time.Minute)
	account.Task = &core.MergedTask{AccountID: "a", Generation: "g", State: core.TaskPending, Intents: []core.Intent{{ID: "stored", Reason: core.ReasonFiveHour, OriginalDueAt: reset, RandomizedDueAt: due, Status: core.IntentPending}}}
	engine.state.Accounts["a"] = account
	plan := engine.schedulePlan(engine.state, clock.now)
	var stored *PlannedAction
	dailyCount := 0
	for index := range plan.Entries {
		entry := &plan.Entries[index]
		if entry.Trigger == string(core.ReasonFiveHour) {
			stored = entry
		}
		if entry.Trigger == string(core.ReasonDaily) {
			dailyCount++
			if !entry.EarliestAt.Equal(entry.LatestAt) || entry.RuleID != "point" {
				t.Fatal("fixed daily time was turned into a random range")
			}
		}
	}
	if stored == nil || stored.Status != "scheduled" || !stored.EarliestAt.Equal(due) || dailyCount != 2 {
		t.Fatalf("persisted deadline or horizon changed: %+v", plan)
	}
	if !reflect.DeepEqual(plan, engine.schedulePlan(engine.state, clock.now)) {
		t.Fatal("reading the plan resampled random dates")
	}
}

func TestPlanIsBoundedSortedAndItsPublishedEntriesAreIndependent(t *testing.T) {
	engine, _, _, clock := fixture(t, false, false)
	engine.cfg.Planning.Enabled = true
	engine.cfg.Enabled = false
	engine.state.Accounts = map[core.AccountID]core.AccountState{}
	for index := 0; index < maximumPlanEntries+20; index++ {
		id := core.AccountID(fmt.Sprintf("account-%03d", index))
		engine.state.Accounts[id] = core.AccountState{ID: id, Enabled: true}
	}
	plan := engine.schedulePlan(engine.state, clock.now)
	if len(plan.Entries) != maximumPlanEntries || !plan.Truncated || plan.Entries[0].AccountID != "account-000" {
		t.Fatal("plan was not limited to the earliest deterministic entries")
	}
	engine.publish(true)
	summary := engine.SchedulerDiagnostics().(SchedulingSummary)
	summary.Plan.Entries[0].Detail = "private mutation"
	if engine.SchedulerDiagnostics().(SchedulingSummary).Plan.Entries[0].Detail == "private mutation" {
		t.Fatal("reader modified an immutable published plan")
	}
}

func TestNextAllowedPlanTimeUsesPolicyTimezoneAndCrossMidnight(t *testing.T) {
	settings := config.Defaults()
	settings.Timezone = "America/New_York"
	settings.OrdinaryRules.AllowedHours = []string{"02:00-04:00"}
	from, _ := time.Parse(time.RFC3339, "2026-03-08T01:30:00-05:00")
	allowed, ok := nextAllowedTime(settings, "a", from, from.Add(24*time.Hour))
	if !ok || allowed.In(settings.Location()).Hour() != 3 {
		t.Fatal("plan did not resolve DST in the configured timezone")
	}
	settings.OrdinaryRules.AllowedHours = []string{"22:00-02:00"}
	from, _ = time.Parse(time.RFC3339, "2026-10-13T03:00:00-04:00")
	allowed, ok = nextAllowedTime(settings, "a", from, from.Add(24*time.Hour))
	if !ok || allowed.In(settings.Location()).Hour() != 22 {
		t.Fatal("cross-midnight plan jumped to an invalid time")
	}
}

func TestAllowedPlanTimesHandleBothOccurrencesOfRepeatedDSTHour(t *testing.T) {
	settings := config.Defaults()
	settings.Timezone = "America/New_York"
	for _, scenario := range []struct{ from, hours, expected string }{
		{"2026-11-01T01:50:00-04:00", "01:00-01:40", "2026-11-01T01:00:00-05:00"},
		{"2026-11-01T01:15:00-05:00", "01:30-01:40", "2026-11-01T01:30:00-05:00"},
	} {
		settings.OrdinaryRules.AllowedHours = []string{scenario.hours}
		from, _ := time.Parse(time.RFC3339, scenario.from)
		expected, _ := time.Parse(time.RFC3339, scenario.expected)
		allowed, ok := nextAllowedTime(settings, "a", from, from.Add(24*time.Hour))
		if !ok || !allowed.Equal(expected) {
			t.Fatalf("wrong repeated-hour plan: %s => %v", scenario.from, allowed)
		}
	}
}

func TestNextWakesRemainCompleteWithoutDetailedPlan(t *testing.T) {
	engine, host, _, clock := fixture(t, false, true)
	engine.cfg.Planning.Enabled = false
	engine.cfg.Planning.HorizonDays = 7
	engine.state.Accounts = map[core.AccountID]core.AccountState{}
	count := maximumPlanEntries + 20
	for index := 0; index < count; index++ {
		id := core.AccountID(fmt.Sprintf("account-%03d", index))
		engine.state.Accounts[id] = core.AccountState{ID: id, Enabled: true, FiveHourBaseline: core.WindowBaseline{StableResetAt: clock.now.Add(time.Hour)}, WeeklyBaseline: core.WindowBaseline{StableResetAt: clock.now.Add(24 * time.Hour)}}
	}
	before, _ := json.Marshal(engine.state)
	plan := engine.schedulePlan(engine.state, clock.now)
	if plan.Enabled || len(plan.Entries) != 0 || len(plan.NextWakes) != count {
		t.Fatalf("incomplete compact summary: %d wakes, %d entries", len(plan.NextWakes), len(plan.Entries))
	}
	for id, wake := range plan.NextWakes {
		if wake.AccountID != id || wake.Kind != "wake" || wake.Trigger != string(core.ReasonFiveHour) {
			t.Fatalf("not the earliest account wake: %+v", wake)
		}
	}
	engine.cfg.Planning.Enabled = true
	detailed := engine.schedulePlan(engine.state, clock.now)
	if !detailed.Truncated || !reflect.DeepEqual(detailed.NextWakes, plan.NextWakes) {
		t.Fatal("full timeline truncation changed account summary")
	}
	after, _ := json.Marshal(engine.state)
	if string(before) != string(after) || host.reads != 0 || len(host.sends) != 0 {
		t.Fatal("summary changed state or sent requests")
	}
	engine.publish(true)
	summary := engine.SchedulerDiagnostics().(SchedulingSummary)
	delete(summary.Plan.NextWakes, "account-000")
	if len(engine.SchedulerDiagnostics().(SchedulingSummary).Plan.NextWakes) != count {
		t.Fatal("summary mutation escaped snapshot")
	}
}

func TestNextWakeSummaryOmitsUnavailableAndUnassignedTimes(t *testing.T) {
	for _, scenario := range []string{"disabled", "paused", "verifying", "unknown", "wake_off", "blocked_hours", "daily_pool_only", "stored_deadline", "assigned_daily"} {
		t.Run(scenario, func(t *testing.T) {
			engine, _, _, clock := fixture(t, false, true)
			engine.cfg.Planning.Enabled = false
			a := engine.state.Accounts["a"]
			a.FiveHourBaseline.StableResetAt = clock.now.Add(time.Hour)
			a.WeeklyBaseline = core.WindowBaseline{}
			switch scenario {
			case "disabled":
				a.Enabled = false
			case "paused":
				paused := true
				engine.cfg.AccountOverrides["a"] = config.AccountOverride{Paused: &paused}
			case "verifying":
				a.Task = &core.MergedTask{OperationID: "sent"}
			case "unknown", "daily_pool_only":
				a.FiveHourBaseline = core.WindowBaseline{}
			case "wake_off":
				engine.cfg.Enabled = false
			case "blocked_hours":
				engine.cfg.OrdinaryRules.AllowedHours = nil
			case "assigned_daily":
				engine.cfg.OrdinaryRules.DailyEnabled = true
				engine.cfg.DailyRules = []config.DailyRule{{ID: "point", Enabled: true, RandomTimeRange: "11:00-11:00", MinimumReadyAccounts: 1}}
				a.FiveHourBaseline = core.WindowBaseline{}
				a.Task = &core.MergedTask{Intents: []core.Intent{{Reason: core.ReasonDaily, RuleID: "point", RandomizedDueAt: clock.now.Add(2 * time.Hour), Status: core.IntentPending}}}
			case "stored_deadline":
				a.Task = &core.MergedTask{Intents: []core.Intent{{Reason: core.ReasonFiveHour, OriginalDueAt: a.FiveHourBaseline.StableResetAt, RandomizedDueAt: clock.now.Add(2 * time.Hour), Status: core.IntentPending}}}
			}
			engine.state.Accounts["a"] = a
			plan := engine.schedulePlan(engine.state, clock.now)
			if scenario == "stored_deadline" || scenario == "assigned_daily" {
				wake := plan.NextWakes["a"]
				if wake.Status != "scheduled" || !wake.EarliestAt.Equal(clock.now.Add(2*time.Hour)) || !wake.LatestAt.Equal(wake.EarliestAt) {
					t.Fatalf("lost assigned deadline: %+v", wake)
				}
			} else if len(plan.NextWakes) != 0 {
				t.Fatalf("invented account wake: %+v", plan.NextWakes)
			}
		})
	}
}
