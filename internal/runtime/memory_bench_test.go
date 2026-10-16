package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

// These benchmarks use no upstream requests. They separate allocations in plan
// generation, publication and an idle scheduler with retained request history.
func memoryBenchmarkEngine(accounts, requests, history int) *Engine {
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	cfg := config.Defaults()
	cfg.Enabled = true
	cfg.Execution.Model = "memory-fixture"
	cfg.DailyRules = []config.DailyRule{{ID: "daily", Enabled: true, RandomTimeRange: "09:00-09:10", MinimumReadyAccounts: 1}}
	e := &Engine{cfg: cfg, clock: &testClock{now}, random: fixedRandom{}, wake: make(chan struct{}, 1), nextRead: map[core.AccountID]time.Time{}, nextPreflight: map[core.AccountID]time.Time{}, readFailures: map[core.AccountID]int{}, activityEpoch: map[core.AccountID]uint64{}, nextInventory: now.Add(time.Hour), state: core.CoreState{Accounts: map[core.AccountID]core.AccountState{}, Requests: map[core.RequestID]core.RequestActivity{}, Activity: map[core.UserIdentity]core.UserActivity{}}}
	for index := 0; index < accounts; index++ {
		id := core.AccountID(fmt.Sprintf("memory-account-%04d", index))
		e.state.Accounts[id] = core.AccountState{ID: id, Generation: "g", Enabled: true, OrdinaryParticipant: true, WeeklyEnabled: true,
			FiveHourBaseline: core.WindowBaseline{CycleID: "five", StableResetAt: now.Add(time.Hour)}, WeeklyBaseline: core.WindowBaseline{CycleID: "week", StableResetAt: now.Add(24 * time.Hour)},
			Task: &core.MergedTask{AccountID: id, Generation: "g", State: core.TaskWaiting, Intents: []core.Intent{{ID: "daily", RuleID: "daily", CycleID: "2026-10-16", Reason: core.ReasonDaily, Status: core.IntentPending, OriginalDueAt: now.Add(-time.Hour), RandomizedDueAt: now.Add(-57 * time.Minute)}}}}
		e.inventory = append(e.inventory, core.HostAccount{ID: id, Generation: "g", Enabled: true})
	}
	for index := 0; index < requests; index++ {
		id := core.RequestID(fmt.Sprintf("request-%06d", index))
		e.state.Requests[id] = core.RequestActivity{Completed: true, LastEventAt: now.Add(-time.Hour)}
	}
	for index := 0; index < history; index++ {
		e.state.History = append(e.state.History, core.HistoryEntry{Kind: "wake", AccountID: "memory-account-0000", StartedAt: now.Add(-time.Hour), Status: "verified", Prompt: strings.Repeat("x", 1024)})
	}
	store := &memoryStore{s: cloneState(e.state)}
	e.store = store
	e.host = &fakeHost{clock: e.clock.(*testClock), store: store, initial: e.inventory}
	return e
}

func BenchmarkSchedulePlanMemory(b *testing.B) {
	for _, detailed := range []bool{false, true} {
		b.Run(fmt.Sprintf("accounts=1000/detailed=%t", detailed), func(b *testing.B) {
			e := memoryBenchmarkEngine(1000, 0, 0)
			e.cfg.Planning.Enabled = detailed
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				_ = e.schedulePlan(e.state, e.clock.Now())
			}
		})
	}
}

func BenchmarkPublishMemory(b *testing.B) {
	e := memoryBenchmarkEngine(100, 10000, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		e.publish(true)
	}
}

func BenchmarkIdleStepMemory(b *testing.B) {
	e := memoryBenchmarkEngine(100, 10000, 1000)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	e.publish(true)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := e.Step(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
