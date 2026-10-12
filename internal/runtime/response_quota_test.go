package runtime

import (
	"context"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func foregroundQuota(h *fakeHost, source string) core.QuotaObservation {
	o := h.observation("a", "g", true)
	o.Class, o.Source = core.ObservationExecution, source
	o.FiveHour.Remaining = &core.QuotaAmount{Value: 75, Unit: "percent"}
	o.Weekly.Remaining = &core.QuotaAmount{Value: 60, Unit: "percent"}
	return o
}
func TestForegroundQuotaPublishesAndPostponesPollingUntilIdle(t *testing.T) {
	for _, source := range []string{"cpa_response_headers", "cpa_websocket_quota"} {
		t.Run(source, func(t *testing.T) {
			e, h, _, c := fixture(t, false, false)
			e.cfg.Enabled = false
			h.active = true
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			reads := h.reads
			c.now = c.now.Add(29 * time.Second)
			o := foregroundQuota(h, source)
			e.EnqueueObservation(o)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			a := e.Snapshot().Accounts["a"]
			if h.reads != reads || a.FiveHour.Source != source || a.FiveHour.Remaining.Value != 75 || a.Weekly.Remaining.Value != 60 || !a.FiveHour.ConfirmedAt.Equal(c.now) {
				t.Fatal("passive snapshot not published or unnecessary query occurred")
			}
			c.now = c.now.Add(29 * time.Second)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.reads != reads {
				t.Fatal("queried before passive evidence idle deadline")
			}
			c.now = c.now.Add(time.Second)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.reads != reads+1 {
				t.Fatal("idle fallback query never resumed")
			}
		})
	}
}
func TestPartialUnknownOldOrReplacementQuotaDoesNotSuppressFallback(t *testing.T) {
	for _, kind := range []string{"partial", "unknown", "expired", "exhausted", "stale", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			e, h, _, c := fixture(t, false, false)
			e.cfg.Enabled = false
			h.active = true
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := e.nextRead["a"]
			o := foregroundQuota(h, "cpa_response_headers")
			c.now = c.now.Add(29 * time.Second)
			o.ReceivedAt = c.now
			switch kind {
			case "partial":
				o.Weekly = nil
			case "unknown":
				o.FiveHour.State = core.WindowUnknown
			case "expired":
				o.FiveHour.State = core.WindowExpired
				o.FiveHour.ResetAt = c.now.Add(-time.Minute)
			case "stale":
				o.ReceivedAt = c.now.Add(-time.Hour)
			case "exhausted":
				o.FiveHour.Remaining.Value = 0
			case "replacement":
				o.Generation = "old-credential"
			}
			e.EnqueueObservation(o)
			e.drain()
			if !e.nextRead["a"].Equal(before) {
				t.Fatal("incomplete/stale quota postponed fallback")
			}
			if kind == "partial" && e.state.Accounts["a"].Weekly.Remaining != nil {
				t.Fatal("partial evidence erased or invented weekly data")
			}
			reads := h.reads
			c.now = c.now.Add(time.Second)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.reads != reads+1 {
				t.Fatal("due fallback query was skipped")
			}
		})
	}
}
func TestInFlightQueryCannotOverwriteForegroundQuotaOrReportFalseFailure(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	e.cfg.Enabled = false
	h.active = true
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(30 * time.Second)
	h.queryHook = func() { e.EnqueueObservation(foregroundQuota(h, "cpa_websocket_quota")) }
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := e.Snapshot().Accounts["a"]
	if a.FiveHour.Source != "cpa_websocket_quota" || a.FiveHour.Remaining.Value != 75 || a.Weekly.Remaining.Value != 60 {
		t.Fatal("old query overwrote response quota")
	}
	if e.Status().LastError != "" || len(e.readFailures) != 0 {
		t.Fatal("superseded query treated as adapter failure")
	}
}
func TestForegroundSnapshotCancelsDueWakeWithoutExtraModelRequest(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	e.EnqueueObservation(foregroundQuota(h, "cpa_response_headers"))
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 || h.reads != 0 {
		t.Fatal("fresh active response failed to suppress duplicate wake/query")
	}
	a := e.Snapshot().Accounts["a"]
	if a.FiveHour.State != core.WindowActive || a.FiveHour.Remaining.Value != 75 {
		t.Fatal("foreground quota not published")
	}
}

func TestForegroundQuotaDoesNotDelayUnresolvedOperationVerification(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	a := e.state.Accounts["a"]
	a.Task = &core.MergedTask{AccountID: "a", Generation: "g", OperationID: "pending-op", State: core.TaskAwaitingEvidence, NextVerificationAt: c.now.Add(time.Second), CoveredEventIDs: []core.EventID{"weekly"}, Intents: []core.Intent{{ID: "weekly", Reason: core.ReasonWeekly, Status: core.IntentPending, OriginalDueAt: c.now.Add(time.Hour)}}}
	e.state.Accounts["a"] = a
	e.nextRead["a"] = c.now
	e.EnqueueObservation(foregroundQuota(h, "cpa_websocket_quota"))
	e.drain()
	current := e.state.Accounts["a"]
	if current.Task.OperationID != "pending-op" || !current.Task.NextVerificationAt.Equal(c.now.Add(time.Second)) || !e.nextRead["a"].Equal(c.now) {
		t.Fatal("passive quota postponed or cleared unresolved verification")
	}
}
