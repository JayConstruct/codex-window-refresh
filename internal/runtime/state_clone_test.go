package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestCloneStateDetachesEveryMutableContainer(t *testing.T) {
	now := time.Now()
	source := core.CoreState{
		Accounts:           map[core.AccountID]core.AccountState{"a": {ID: "a", FiveHour: core.WindowEvidence{Remaining: &core.QuotaAmount{Value: 70}}, Weekly: core.WindowEvidence{Remaining: &core.QuotaAmount{Value: 80}}, Task: &core.MergedTask{Intents: []core.Intent{{ID: "event"}}, CoveredEventIDs: []core.EventID{"event"}}}},
		DailyIntents:       []core.PoolDailyIntent{{Intent: core.Intent{ID: "daily"}}},
		Reservations:       []core.Reservation{{ID: "reservation"}},
		Activity:           map[core.UserIdentity]core.UserActivity{"user": {Identity: "user"}},
		Requests:           map[core.RequestID]core.RequestActivity{"request": {Completed: true}},
		History:            []core.HistoryEntry{{Prompt: "prompt", StartedAt: now, Sources: []core.ActionSource{{Trigger: "trigger"}}, CoveredEvents: []core.EventID{"event"}}},
		GenerationJob:      &core.PromptGenerationJob{Candidates: []string{"candidate"}},
		GenerationAccounts: map[core.AccountID]core.GenerationAccount{"g": {ID: "g"}},
	}
	before, _ := json.Marshal(source)
	copied := cloneState(source)
	if !reflect.DeepEqual(source, copied) || copied.History[0].StartedAt.Location() != now.Location() {
		t.Fatal("copy changed state or copied immutable time locations")
	}
	copied.Accounts["a"].FiveHour.Remaining.Value = 0
	copied.Accounts["a"].Weekly.Remaining.Value = 0
	copied.Accounts["a"].Task.Intents[0].ID = "changed"
	copied.Accounts["a"].Task.CoveredEventIDs[0] = "changed"
	delete(copied.Accounts, "a")
	copied.DailyIntents[0].ID = "changed"
	copied.Reservations[0].ID = "changed"
	delete(copied.Activity, "user")
	delete(copied.Requests, "request")
	copied.History[0].Prompt = "changed"
	copied.History[0].Sources[0].Trigger = "changed"
	copied.History[0].CoveredEvents[0] = "changed"
	copied.GenerationJob.Phase = "changed"
	copied.GenerationJob.Candidates[0] = "changed"
	delete(copied.GenerationAccounts, "g")
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("copy retained mutable aliases")
	}
	if !reflect.DeepEqual(core.CoreState{}, cloneState(core.CoreState{})) {
		t.Fatal("nil containers changed")
	}
}

func TestStateDigestDetectsChangesAndDoesNotCommitAnIdleTurn(t *testing.T) {
	e := memoryBenchmarkEngine(2, 10, 10)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	// Let the first reconciliation settle inventory and histories.
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	revision := e.state.Revision
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.state.Revision != revision {
		t.Fatal("unchanged state caused another disk write")
	}
	digest := stateDigest(e.state)
	e.state.History[0].Prompt = "changed"
	if digest == stateDigest(e.state) {
		t.Fatal("historical changes were omitted from change detection")
	}
}

func TestPrunedHistoryDoesNotRetainPromptsInBackingArray(t *testing.T) {
	e := memoryBenchmarkEngine(0, 0, maximumHistoryEntries+2)
	backing := e.state.History
	backing[0].StartedAt = e.clock.Now().Add(-historyRetention - time.Second)
	backing[1].Prompt = "also dropped by count limit"
	e.reconcileHistory(e.clock.Now())
	if len(e.state.History) != maximumHistoryEntries {
		t.Fatal("wrong history limit")
	}
	for _, entry := range backing[len(e.state.History):] {
		if !reflect.DeepEqual(entry, core.HistoryEntry{}) {
			t.Fatal("pruned history remains reachable")
		}
	}
}
