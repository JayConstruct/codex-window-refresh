package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/storage"
)

type dailyTestRandom struct {
	calls            int
	value            time.Duration
	failure          error
	minimum, maximum time.Duration
}

func (r *dailyTestRandom) Duration(minimum, maximum time.Duration) (time.Duration, error) {
	r.calls++
	r.minimum, r.maximum = minimum, maximum
	return r.value, r.failure
}
func dailyTestConfig() config.Config {
	cfg := config.Defaults()
	cfg.DailyRules = []config.DailyRule{{ID: "morning", Enabled: true, RandomTimeRange: "09:00-09:10", MinimumReadyAccounts: 2, CatchUp: true}}
	return cfg
}
func dailyInstant(t *testing.T, value string) time.Time {
	t.Helper()
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return instant
}

func TestDailyRandomSamplePersistedOnceAndHalfOpen(t *testing.T) {
	cfg := dailyTestConfig()
	now := dailyInstant(t, "2026-10-11T08:00:00+08:00")
	rng := &dailyTestRandom{value: 10*time.Minute - time.Nanosecond}
	state := core.CoreState{}
	if err := reconcileDaily(&state, cfg, now, rng); err != nil {
		t.Fatal(err)
	}
	if rng.calls != 1 || rng.minimum != 0 || rng.maximum != 10*time.Minute-time.Nanosecond {
		t.Fatalf("sample bounds/calls = %#v", rng)
	}
	if len(state.DailyIntents) != 1 {
		t.Fatalf("daily intents = %#v", state.DailyIntents)
	}
	intent := state.DailyIntents[0]
	expected := dailyInstant(t, "2026-10-11T09:10:00+08:00").Add(-time.Nanosecond)
	if intent.ID != "daily:morning:2026-10-11" || intent.CycleID != "2026-10-11" || intent.RuleID != "morning" || intent.MinimumReadyAccounts != 2 || !intent.RandomizedDueAt.Equal(expected) {
		t.Fatalf("intent = %#v", intent)
	}
	if err := reconcileDaily(&state, cfg, now.Add(30*time.Minute), rng); err != nil {
		t.Fatal(err)
	}
	if rng.calls != 1 || !state.DailyIntents[0].RandomizedDueAt.Equal(expected) {
		t.Fatal("timer was sampled twice")
	}
	state.DailyIntents[0].Status = core.IntentSatisfied
	if err := reconcileDaily(&state, cfg, now.Add(3*time.Hour), rng); err != nil {
		t.Fatal(err)
	}
	if rng.calls != 1 || len(state.DailyIntents) != 1 || state.DailyIntents[0].Status != core.IntentSatisfied {
		t.Fatal("satisfied timer resurrected")
	}
}

func TestDailyLocalDateFirstDeploymentAndNextDay(t *testing.T) {
	cfg := dailyTestConfig()
	rng := &dailyTestRandom{}
	state := core.CoreState{}
	// UTC date is October 10; configured civil date is October 11.
	now := dailyInstant(t, "2026-10-10T17:00:00Z")
	if err := reconcileDaily(&state, cfg, now, rng); err != nil {
		t.Fatal(err)
	}
	if len(state.DailyIntents) != 1 || state.DailyIntents[0].CycleID != "2026-10-11" {
		t.Fatalf("wrong initial date/history: %#v", state.DailyIntents)
	}
	if err := reconcileDaily(&state, cfg, now.AddDate(0, 0, 4), rng); err != nil {
		t.Fatal(err)
	}
	if len(state.DailyIntents) != 2 || state.DailyIntents[1].CycleID != "2026-10-15" || rng.calls != 2 {
		t.Fatalf("reconstructed absent historical dates: %#v", state.DailyIntents)
	}
	if state.DailyIntents[0].Status != core.IntentPending {
		t.Fatal("persisted catch-up intent was discarded")
	}
}

func TestDailyPersistedPendingSurvivesRestart(t *testing.T) {
	cfg := dailyTestConfig()
	cfg.DailyRules[0].RandomTimeRange = "09:00-09:00"
	now := dailyInstant(t, "2026-10-11T08:00:00+08:00")
	state := core.CoreState{}
	if err := reconcileDaily(&state, cfg, now, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), 0, state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileDaily(&restored, cfg, now.Add(5*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if len(restored.DailyIntents) != 1 || restored.DailyIntents[0].Status != core.IntentPending || !restored.DailyIntents[0].RandomizedDueAt.Equal(dailyInstant(t, "2026-10-11T09:00:00+08:00")) {
		t.Fatalf("lost catch-up intent: %#v", restored.DailyIntents)
	}
}

func TestDailyDisabledDeletedAndPausedRules(t *testing.T) {
	cfg := dailyTestConfig()
	now := dailyInstant(t, "2026-10-11T08:00:00+08:00")
	for _, mode := range []string{"disabled", "deleted", "paused"} {
		t.Run(mode, func(t *testing.T) {
			current := cfg
			current.DailyRules = append([]config.DailyRule(nil), cfg.DailyRules...)
			state := core.CoreState{}
			rng := &dailyTestRandom{}
			if err := reconcileDaily(&state, current, now, rng); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "disabled":
				current.DailyRules[0].Enabled = false
			case "deleted":
				current.DailyRules = nil
			case "paused":
				current.Enabled = false
			}
			if err := reconcileDaily(&state, current, now.Add(time.Hour), rng); err != nil {
				t.Fatal(err)
			}
			expected := core.IntentSuperseded
			if mode == "paused" {
				expected = core.IntentPending
			}
			if len(state.DailyIntents) != 1 || state.DailyIntents[0].Status != expected || rng.calls != 1 {
				t.Fatalf("state = %#v", state.DailyIntents)
			}
		})
	}
}

func TestDailyCatchUpDisabledSkipsMissedRange(t *testing.T) {
	for _, mode := range []string{"global", "rule", "both", "neither"} {
		t.Run(mode, func(t *testing.T) {
			cfg := dailyTestConfig()
			cfg.Execution.CatchUp = mode != "global" && mode != "both"
			cfg.DailyRules[0].CatchUp = mode != "rule" && mode != "both"
			state := core.CoreState{}
			rng := &dailyTestRandom{}
			if err := reconcileDaily(&state, cfg, dailyInstant(t, "2026-10-11T09:10:00+08:00"), rng); err != nil {
				t.Fatal(err)
			}
			expected := core.IntentSuperseded
			if mode == "neither" {
				expected = core.IntentPending
			}
			if len(state.DailyIntents) != 1 || state.DailyIntents[0].Status != expected {
				t.Fatalf("state = %#v", state.DailyIntents)
			}
			if err := reconcileDaily(&state, cfg, dailyInstant(t, "2026-10-11T09:05:00+08:00"), rng); err != nil {
				t.Fatal(err)
			}
			if rng.calls != 1 || state.DailyIntents[0].Status != expected {
				t.Fatal("clock rollback resurrected skipped timer")
			}
		})
	}
}

func TestDailyExactScheduleAllowsScheduledMinute(t *testing.T) {
	cfg := dailyTestConfig()
	cfg.Execution.CatchUp = false
	cfg.DailyRules[0].RandomTimeRange = "09:00-09:00"
	state := core.CoreState{}
	if err := reconcileDaily(&state, cfg, dailyInstant(t, "2026-10-11T09:00:30+08:00"), nil); err != nil {
		t.Fatal(err)
	}
	if state.DailyIntents[0].Status != core.IntentPending {
		t.Fatal("exact timer expired within its minute")
	}
	if err := reconcileDaily(&state, cfg, dailyInstant(t, "2026-10-11T09:01:00+08:00"), nil); err != nil {
		t.Fatal(err)
	}
	if state.DailyIntents[0].Status != core.IntentSuperseded {
		t.Fatal("missed exact timer remained pending")
	}
}

func TestDailyDSTWallResolution(t *testing.T) {
	tests := []struct {
		zone, date string
		offset     time.Duration
		expected   string
	}{
		{"America/New_York", "2026-11-01", 90 * time.Minute, "2026-11-01T05:30:00Z"},
		{"America/New_York", "2026-03-08", 150*time.Minute + 15*time.Second, "2026-03-08T07:00:00Z"},
		{"Australia/Lord_Howe", "2026-10-04", 135 * time.Minute, "2026-10-03T15:30:00Z"},
		{"Pacific/Apia", "2011-12-30", 9 * time.Hour, "2011-12-30T10:00:00Z"},
		{"Asia/Shanghai", "2026-10-11", 24 * time.Hour, "2026-10-11T16:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.zone+"/"+test.date, func(t *testing.T) {
			loc, err := time.LoadLocation(test.zone)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := resolveDailyWallTime(test.date, test.offset, loc)
			if err != nil || !actual.Equal(dailyInstant(t, test.expected)) {
				t.Fatalf("resolved %s %s to %s, %v; want %s", test.zone, test.date, actual, err, test.expected)
			}
		})
	}
}

func TestDailyDSTGapDoesNotExpireBeforeResolvedExecution(t *testing.T) {
	cfg := dailyTestConfig()
	cfg.Timezone = "America/New_York"
	cfg.Execution.CatchUp = false
	cfg.DailyRules[0].RandomTimeRange = "02:00-02:30"
	state := core.CoreState{}
	now := dailyInstant(t, "2026-03-08T07:00:00Z")
	if err := reconcileDaily(&state, cfg, now, &dailyTestRandom{value: 15 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if state.DailyIntents[0].Status != core.IntentPending || !state.DailyIntents[0].RandomizedDueAt.Equal(now) {
		t.Fatalf("gap timer expired prematurely: %#v", state.DailyIntents)
	}
	if err := reconcileDaily(&state, cfg, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if state.DailyIntents[0].Status != core.IntentSuperseded {
		t.Fatal("gap timer failed to expire")
	}
}

func TestDailyBoundedHistoryPreservesNewestPending(t *testing.T) {
	cfg := dailyTestConfig()
	cfg.DailyRules[0].RandomTimeRange = "09:00-09:00"
	start := dailyInstant(t, "2026-10-01T08:00:00+08:00")
	state := core.CoreState{}
	for day := 0; day < 20; day++ {
		if err := reconcileDaily(&state, cfg, start.AddDate(0, 0, day), nil); err != nil {
			t.Fatal(err)
		}
		for i := range state.DailyIntents {
			state.DailyIntents[i].Status = core.IntentSatisfied
		}
	}
	if len(state.DailyIntents) != dailyHistoryPerRule {
		t.Fatalf("unbounded history: %d", len(state.DailyIntents))
	}
	// If the newest remaining pending event predates the newest eight completed
	// dates, keep it by displacing an older terminal history entry.
	state.DailyIntents = append(state.DailyIntents, core.PoolDailyIntent{Intent: core.Intent{
		ID: "daily:morning:2026-10-01", RuleID: "morning", CycleID: "2026-10-01",
		Reason: core.ReasonDaily, Status: core.IntentPending, OriginalDueAt: start.Add(time.Hour),
		RandomizedDueAt: start.Add(time.Hour), CreatedAt: start,
	}})
	if err := reconcileDaily(&state, cfg, start.AddDate(0, 0, 19), nil); err != nil {
		t.Fatal(err)
	}
	if len(state.DailyIntents) != dailyHistoryPerRule {
		t.Fatalf("unbounded history retaining old pending: %d", len(state.DailyIntents))
	}
	pending := 0
	for _, intent := range state.DailyIntents {
		if intent.Status == core.IntentPending {
			pending++
			if intent.CycleID != "2026-10-01" {
				t.Fatal("wrong retained pending date")
			}
		}
	}
	if pending != 1 {
		t.Fatalf("old newest pending was lost: %#v", state.DailyIntents)
	}
	before := append([]core.PoolDailyIntent(nil), state.DailyIntents...)
	if err := reconcileDaily(&state, cfg, start.AddDate(0, 0, 19), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, state.DailyIntents) {
		t.Fatal("bounded history resurrected a satisfied current date")
	}
	if err := reconcileDaily(&state, cfg, start.AddDate(0, 0, 5), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, state.DailyIntents) {
		t.Fatal("clock rollback resurrected an already-pruned daily date")
	}
}

func TestDailyFailureLeavesStateUnchanged(t *testing.T) {
	cfg := dailyTestConfig()
	now := dailyInstant(t, "2026-10-11T08:00:00+08:00")
	existing := core.PoolDailyIntent{Intent: core.Intent{ID: "daily:deleted:2026-10-10", Reason: core.ReasonDaily, RuleID: "deleted", CycleID: "2026-10-10", Status: core.IntentPending}}
	for _, rng := range []*dailyTestRandom{{failure: errors.New("no entropy")}, {value: -time.Second}, {value: 10 * time.Minute}} {
		state := core.CoreState{DailyIntents: []core.PoolDailyIntent{existing}}
		before := append([]core.PoolDailyIntent(nil), state.DailyIntents...)
		if err := reconcileDaily(&state, cfg, now, rng); err == nil {
			t.Fatal("invalid random sample accepted")
		}
		if !reflect.DeepEqual(before, state.DailyIntents) {
			t.Fatal("failed transition mutated pending state")
		}
	}
}
