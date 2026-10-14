package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func TestWakeRuleUsesItsOwnModelAndContent(t *testing.T) {
	for _, scenario := range []string{"five_hour", "weekly", "daily", "manual"} {
		t.Run(scenario, func(t *testing.T) {
			engine, host, _, clock := fixture(t, false, scenario == "weekly")
			if scenario == "manual" {
				engine, host, _, clock = manualWakeFixture(t)
				engine.QueueManualWake("a")
			}
			engine.cfg.ResetRules.FiveHour.Model, engine.cfg.ResetRules.FiveHour.Prompt = "five-hour-model", "Five hour message"
			engine.cfg.Execution.PromptMode = "fixed"
			engine.cfg.ResetRules.Weekly.Model, engine.cfg.ResetRules.Weekly.Prompt = "weekly-model", "Weekly message"
			if scenario == "five_hour" {
				addUser(engine, clock)
			}
			if scenario == "daily" {
				engine.cfg.ResetRules.FiveHour.Enabled = false
				engine.cfg.DailyRules = []config.DailyRule{{ID: "point", Enabled: true, RandomTimeRange: "09:00-09:00", MinimumReadyAccounts: 1, CatchUp: true}}
			}
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(host.sends) != 1 {
				t.Fatalf("expected one request, got %d", len(host.sends))
			}
			model, prompt := "five-hour-model", "Five hour message"
			if scenario == "weekly" {
				model, prompt = "weekly-model", "Weekly message"
			}
			if scenario == "manual" {
				model, prompt = engine.cfg.Execution.Model, engine.cfg.Execution.Prompt
			}
			if host.sends[0].Model != model || host.sends[0].Prompt != prompt {
				t.Fatalf("wrong rule content: %+v", host.sends[0])
			}
		})
	}
}

func TestDailySwitchDisablesPendingSchedulesWithoutDeletingRules(t *testing.T) {
	engine, host, _, clock := fixture(t, false, false)
	clock.now = clock.now.Add(-2 * time.Hour)
	engine.cfg.QuotaQuery.Automatic = false
	engine.cfg.ResetRules.FiveHour.Enabled = false
	engine.cfg.DailyRules = []config.DailyRule{{ID: "point", Enabled: true, RandomTimeRange: "09:00-09:00", MinimumReadyAccounts: 1, CatchUp: true}}
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.state.DailyIntents) != 1 || engine.state.DailyIntents[0].Status != core.IntentPending || len(host.sends) != 0 {
		t.Fatal("daily future target was not allocated")
	}
	settings := engine.cfg
	settings.OrdinaryRules.DailyEnabled = false
	if err := engine.UpdateConfig(settings); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Hour)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 0 || engine.state.DailyIntents[0].Status != core.IntentSuperseded || len(engine.cfg.DailyRules) != 1 {
		t.Fatal("disabled daily schedule dispatched or deleted the editable rule")
	}
}
