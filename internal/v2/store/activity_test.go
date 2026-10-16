package store

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/events"
)

func batch(after uint64, list ...events.Event) events.Batch {
	latest := after
	if len(list) > 0 {
		latest = list[len(list)-1].Sequence
	}
	return events.Batch{Instance: "bridge-one", HostProcess: "host-one", After: after, Earliest: 1, Latest: latest, Events: list}
}
func event(seq uint64, kind, id, user, account string) events.Event {
	return events.Event{Sequence: seq, Kind: kind, RequestID: id, Identity: user, AccountID: account, At: time.Now().UTC()}
}
func TestLifecycleReplayAndUnknownFinishDoNotManufactureUsers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	start := batch(0, event(1, "started", "r", "u", ""), event(2, "account_selected", "r", "u", "a"))
	if seq, err := s.ApplyEvents(ctx, start); err != nil || seq != 2 {
		t.Fatal(seq, err)
	}
	before, err := s.Overview(ctx)
	if err != nil || before.Activity.RecentUsers != 1 || before.Activity.ActiveRequests != 1 || !before.Activity.Connected || before.Activity.BaselineComplete {
		t.Fatalf("view=%+v err=%v", before, err)
	}
	if _, err = s.ApplyEvents(ctx, start); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Overview(ctx)
	if after.Activity.ActiveRequests != 1 || after.Revision != before.Revision {
		t.Fatal("replay changed committed activity")
	}
	finish := batch(2, event(3, "completed", "r", "u", ""), event(4, "completed", "unknown", "bogus", ""))
	if _, err = s.ApplyEvents(ctx, finish); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Overview(ctx)
	if view.Activity.RecentUsers != 1 || view.Activity.ActiveRequests != 0 {
		t.Fatal("unknown finish manufactured user", view)
	}
	// A delayed duplicate start cannot reopen a completed request.
	if _, err = s.ApplyEvents(ctx, batch(4, event(5, "started", "r", "u", ""))); err != nil {
		t.Fatal(err)
	}
	view, _ = s.Overview(ctx)
	if view.Activity.ActiveRequests != 0 {
		t.Fatal("completed request reopened")
	}
}
func TestGapPreservesUnknownOccupancyAndSurvivesReconnect(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.ApplyEvents(ctx, batch(0, event(1, "started", "r", "u", "a"))); err != nil {
		t.Fatal(err)
	}
	gap := batch(1, event(3, "started", "second", "v", "b"))
	gap.Earliest = 3
	gap.Gap = true
	gap.LossEpoch = 1
	if _, err := s.ApplyEvents(ctx, gap); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Overview(ctx)
	if view.Activity.Status != "gap" || view.Activity.UnknownRequests != 1 || view.Activity.ActiveRequests != 1 {
		t.Fatal(view)
	}
	if _, err := s.ApplyEvents(ctx, gap); err != nil {
		t.Fatal(err)
	}
	replayed, _ := s.Overview(ctx)
	if replayed.Revision != view.Revision || replayed.Activity.ActiveRequests != 1 || replayed.Activity.UnknownRequests != 1 {
		t.Fatal("replayed gap changed requests observed after it", replayed)
	}
	if err := s.EventDisconnected(ctx, "bridge_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEvents(ctx, batchWithEpoch(3, 1)); err != nil {
		t.Fatal(err)
	}
	view, _ = s.Overview(ctx)
	if !view.Activity.Connected || view.Activity.Status != "gap" || view.Activity.UnknownRequests != 1 {
		t.Fatal("reconnect cleared unknown occupancy", view)
	}
	invalid := batch(3, event(5, "completed", "r", "u", ""))
	invalid.LossEpoch = 1
	if _, err := s.ApplyEvents(ctx, invalid); err == nil {
		t.Fatal("unexplained gap accepted")
	}
	cursor, _ := s.EventCursor(ctx)
	if cursor.Sequence != 3 {
		t.Fatal("failed batch advanced cursor")
	}
}

func TestCompletionAfterClockRollbackAndSelectionWithoutScope(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	start := event(1, "started", "r", "u", "a")
	finish := event(2, "completed", "r", "", "")
	finish.At = start.At.Add(-time.Minute)
	if _, err := s.ApplyEvents(ctx, batch(0, start, finish, event(3, "account_selected", "no-scope", "", "a"))); err != nil {
		t.Fatal(err)
	}
	view, err := s.Overview(ctx)
	if err != nil || view.Activity.ActiveRequests != 0 || view.Activity.RecentUsers != 1 {
		t.Fatal("clock rollback or unscoped selection manufactured occupancy", view, err)
	}
}
func batchWithEpoch(after, epoch uint64) events.Batch {
	b := batch(after)
	b.LossEpoch = epoch
	return b
}
func TestBridgeReloadAndHostRestartHaveDifferentRecovery(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.ApplyEvents(ctx, batch(0, event(1, "started", "r", "u", "a"))); err != nil {
		t.Fatal(err)
	}
	reload := batch(0)
	reload.Instance = "bridge-two"
	if _, err := s.ApplyEvents(ctx, reload); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Overview(ctx)
	if view.Activity.UnknownRequests != 1 || view.Activity.Status != "gap" {
		t.Fatal("same-process reload released occupancy")
	}
	if _, err := s.ApplyEvents(ctx, batch(0)); err == nil {
		t.Fatal("retired bridge became current again")
	}
	restart := reload
	restart.Instance = "bridge-three"
	restart.HostProcess = "host-two"
	if _, err := s.ApplyEvents(ctx, restart); err != nil {
		t.Fatal(err)
	}
	view, _ = s.Overview(ctx)
	if view.Activity.UnknownRequests != 0 || view.Activity.Status != "observing" || view.Activity.BaselineComplete {
		t.Fatal("host restart recovery invalid", view)
	}
}
func TestMissingCompletionBecomesUnknownWithoutKeepingUserActiveForever(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old := event(1, "started", "r", "u", "a")
	old.At = time.Now().Add(-25 * time.Hour)
	if _, err := s.ApplyEvents(ctx, batch(0, old)); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, time.Now(), DefaultRetention()); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Overview(ctx)
	if view.Activity.UnknownRequests != 1 || view.Activity.ActiveRequests != 0 || view.Activity.RecentUsers != 0 || view.Activity.Reason != "missing_completion" {
		t.Fatal(view)
	}
}
