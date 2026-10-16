package store

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/quota"
)

func passive(seq uint64, binding string, at time.Time) events.Event {
	return events.Event{Sequence: seq, Kind: "quota_observed", RequestID: "r", AccountID: "a", AuthIndex: "index", Binding: binding, Source: "cpa_response_headers", At: at, Headers: http.Header{"X-Codex-Allowed": {"true"}, "X-Codex-Primary-Used-Percent": {"40"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"3000"}}}
}
func TestPassiveCommitReplayAndReplacementFence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := Account{ID: "a", AuthIndex: "index", Binding: "one", Status: "available"}
	if err := s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	b := batch(0, passive(1, "one", time.Now().UTC()))
	if _, err := s.ApplyEvents(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, _ := s.AccountPage(ctx, "", 10, nil)
	if len(page.Accounts) != 1 || len(page.Accounts[0].Quota) == 0 {
		t.Fatal("passive observation missing")
	}
	if _, err := s.ApplyEvents(ctx, b); err != nil {
		t.Fatal(err)
	}
	replay, _ := s.AccountPage(ctx, "", 10, nil)
	if replay.Revision != page.Revision {
		t.Fatal("replay changed evidence")
	}
	a.Binding = "two"
	if err := s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEvents(ctx, batch(1, passive(2, "one", time.Now().UTC()))); err != nil {
		t.Fatal(err)
	}
	page, _ = s.AccountPage(ctx, "", 10, nil)
	if len(page.Accounts[0].Quota) != 0 {
		t.Fatal("old response accepted after replacement")
	}
	if _, err := s.ApplyEvents(ctx, batch(2, passive(3, "two", time.Now().UTC()))); err != nil {
		t.Fatal(err)
	}
	page, _ = s.AccountPage(ctx, "", 10, nil)
	if len(page.Accounts[0].Quota) == 0 {
		t.Fatal("new binding not accepted")
	}
}
func TestSlowQueryCannotOverwriteNewerPassiveWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := Account{ID: "a", AuthIndex: "index", Binding: "one", Status: "available"}
	if err := s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	job := createTest(t, s, "slow", "a")
	if err := s.Expand(ctx, job, []Account{a}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	if _, err = s.ApplyEvents(ctx, batch(0, passive(1, "one", time.Now().UTC()))); err != nil {
		t.Fatal(err)
	}
	result, err := quota.Normalize([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"window_minutes":300,"reset_after_seconds":3000}}}`), nil, "one", "cpa_wham_usage", time.Now().UTC(), started)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if err = s.Finish(ctx, run, raw, ""); err != nil {
		t.Fatal(err)
	}
	page, _ := s.AccountPage(ctx, "", 10, nil)
	var saved quota.Snapshot
	_ = json.Unmarshal(page.Accounts[0].Quota, &saved)
	if saved.FiveHour.Remaining.Value != 60 {
		t.Fatal("slow query overwrote execution", string(page.Accounts[0].Quota))
	}
	j, _ := s.Job(ctx, job.ID)
	if j.Phase != "completed" {
		t.Fatal("successful read lost its job outcome")
	}
}
