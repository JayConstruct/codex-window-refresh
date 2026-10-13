package runtime

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

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
