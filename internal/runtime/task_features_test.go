package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

func TestUsageGateIsPoolWideAndDoesNotCompleteBlockedBoundary(t *testing.T) {
	engine, host, _, clock := fixture(t, false, false)
	engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
	engine.cfg.Planning.Enabled = true
	engine.cfg.QuotaQuery.OnFiveHourExpiry = true
	engine.cfg.QuotaQuery.FiveHourRecentUsage.Enabled = true
	engine.cfg.QuotaQuery.FiveHourRecentUsage.Lookback = config.Duration(2 * time.Hour)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 0 || !engine.state.Accounts["a"].FiveHourExpiryQueried.IsZero() {
		t.Fatal("usage gate queried or marked blocked cycle")
	}
	plan := engine.schedulePlan(engine.state, clock.now)
	if len(plan.Entries) != 1 || plan.Entries[0].Status != "blocked" || len(plan.Entries[0].Sources) != 1 {
		t.Fatal("plan missing gate explanation or source")
	}
	engine.EnqueueActivity(core.ActivityEvent{RequestID: "other-user", Identity: "real-user", At: clock.now.Add(-time.Hour), Phase: core.ActivityCompletionOnly, AccountID: "different-account", Generation: "different"})
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.reads != 1 || len(engine.state.Activity) != 1 {
		t.Fatal("pool activity was pruned by shorter capacity lookback or did not permit query")
	}
}

func TestUsageGateDoesNotAffectOtherQueriesAndCountsInflight(t *testing.T) {
	for _, kind := range []string{"manual", "periodic", "weekly", "inflight"} {
		engine, host, _, clock := fixture(t, false, true)
		engine.cfg.Enabled = false
		engine.cfg.QuotaQuery.FiveHourRecentUsage.Enabled = true
		engine.cfg.QuotaQuery.Automatic = kind == "periodic"
		engine.cfg.QuotaQuery.OnWeeklyExpiry = kind == "weekly"
		engine.cfg.QuotaQuery.OnFiveHourExpiry = kind == "inflight"
		if kind == "manual" {
			engine.RequestQuotaQuery()
		}
		if kind == "inflight" {
			engine.EnqueueActivity(core.ActivityEvent{RequestID: "long", Identity: "real-user", At: clock.now.Add(-time.Hour), Phase: core.ActivityStarted})
		}
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.reads != 1 || len(host.sends) != 0 {
			t.Fatalf("usage gate blocked independent %s", kind)
		}
	}
}

func TestRandomPromptAndHistoryPersistBeforeNetworkAndAcrossRestart(t *testing.T) {
	engine, host, store, clock := manualWakeFixture(t)
	engine.cfg.Execution.PromptPool = []string{"first", "second"}
	engine.cfg.Execution.Prompt = "must not migrate"
	engine.random = fixedRandom{n: 1}
	engine.QueueManualWake("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 || host.sends[0].Prompt != "second" {
		t.Fatal("random pool not used")
	}
	var wake core.HistoryEntry
	for _, entry := range engine.state.History {
		if entry.Kind == "wake" {
			wake = entry
		}
	}
	if wake.Prompt != "second" || wake.OperationID == "" || len(wake.Sources) != 1 || wake.Sources[0].Trigger != "manual_wake" {
		t.Fatalf("wake history incomplete: %+v", wake)
	}
	restarted, err := New(engine.cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Snapshot().History[len(restarted.Snapshot().History)-1].Prompt != "second" {
		t.Fatal("history lost selected prompt across restart")
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.sends) != 1 {
		t.Fatal("restart repeated randomized request")
	}
}

func TestHistoryIsBoundedFilteredAndPublishedAsIndependentPages(t *testing.T) {
	engine, _, _, clock := fixture(t, false, false)
	engine.state.History = []core.HistoryEntry{{ID: 1, Kind: "query", StartedAt: clock.now.Add(-31 * 24 * time.Hour)}}
	engine.state.NextReadSequence = 1
	for index := 0; index < maximumHistoryEntries+2; index++ {
		core.AppendHistory(&engine.state, core.HistoryEntry{Kind: "query", AccountID: "a", Status: "completed", StartedAt: clock.now, Sources: []core.ActionSource{{Trigger: "manual_query"}}})
	}
	if err := engine.commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.state.History) != maximumHistoryEntries {
		t.Fatal("history retention unbounded")
	}
	page := engine.History(HistoryFilter{AccountID: "a", Kind: "query", Status: "completed", Limit: 2})
	if len(page.Entries) != 2 || page.NextCursor == 0 || page.Entries[0].ID <= page.Entries[1].ID {
		t.Fatal("history pagination not newest first")
	}
	page.Entries[0].Sources[0].Trigger = "mutated"
	if engine.History(HistoryFilter{Limit: 1}).Entries[0].Sources[0].Trigger == "mutated" {
		t.Fatal("history reader mutated durable publication")
	}
	next := engine.History(HistoryFilter{Cursor: page.NextCursor, Limit: 2})
	if len(next.Entries) != 2 || next.Entries[0].ID >= page.Entries[1].ID {
		t.Fatal("cursor repeated entries")
	}
	if len(engine.History(HistoryFilter{Kind: "wake"}).Entries) != 0 {
		t.Fatal("history ignored filters")
	}
}
