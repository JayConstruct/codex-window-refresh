package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestSendRetainsHeaderAndLaterPartialQuotaOnEveryStreamExit(t *testing.T) {
	for _, ending := range []string{"completed", "error_frame", "callback_error", "oversize", "canceled"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			auth := &fakeCallbacks{upstream: "real"}
			reads, sends := 0, 0
			var earliest, latest time.Time
			callback := callbackFunc(func(callCtx context.Context, method string, request, response any) error {
				switch method {
				case "host.model.execute_stream":
					sends++
					earliest = time.Now().UTC()
					return json.Unmarshal([]byte(`{"status_code":200,"stream_id":"partial","headers":{"X-Codex-Primary-Used-Percent":["5"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["3500"]}}`), response)
				case "host.model.stream_read":
					reads++
					if reads == 1 {
						latest = time.Now().UTC()
						// The header is five-hour only; this event is weekly only.
						if ending == "canceled" {
							cancel()
						}
						return respond(response, map[string]any{"payload": []byte(`data: {"type":"codex.rate_limits","rate_limits":{"secondary":{"used_percent":12,"window_minutes":10080,"reset_after_seconds":86000}}}`)})
					}
					switch ending {
					case "callback_error":
						return errors.New("upstream read failed")
					case "oversize":
						return respond(response, map[string]any{"payload": []byte(strings.Repeat("x", (1<<20)+1))})
					case "error_frame":
						return respond(response, map[string]any{"payload": []byte(`data: {"type":"error","message":"rejected"}`), "done": true})
					default:
						return respond(response, map[string]any{"payload": []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`), "done": true})
					}
				default:
					return auth.Call(callCtx, method, request, response)
				}
			})
			host := NewHost(callback, nil)
			accounts, err := host.Accounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			result, err := host.SendExact(ctx, core.ExactRequest{AccountID: accounts[0].ID, Generation: accounts[0].Generation, OperationID: "partial-op", Model: "model", Prompt: "hi"})
			if err != nil || sends != 1 || !auth.closed || len(result.Observations) != 2 {
				t.Fatalf("partial evidence lost: %+v err=%v sends=%d closed=%v", result, err, sends, auth.closed)
			}
			short, week := result.Observations[0], result.Observations[1]
			if short.FiveHour == nil || short.Weekly != nil || week.Weekly == nil || week.FiveHour != nil || short.FiveHour.OperationID != "partial-op" || week.Weekly.OperationID != "partial-op" {
				t.Fatalf("observations combined or uncorrelated: %+v %+v", short, week)
			}
			if short.ReceivedAt.Before(earliest) || short.ReceivedAt.After(latest) || week.ReceivedAt.Before(latest) || short.ReceivedAt.After(week.ReceivedAt) {
				t.Fatalf("observation timestamps refreshed or reordered: header=%s frame=%s before=%s after=%s", short.ReceivedAt, week.ReceivedAt, earliest, latest)
			}
			if result.Observation == nil || result.Observation.FiveHour != nil || result.Observation.Weekly == nil || !result.Observation.ReceivedAt.Equal(week.ReceivedAt) {
				t.Fatal("legacy observation is not the actual latest message")
			}
			codes := map[string]string{"completed": "", "error_frame": "model_stream_failed", "callback_error": "model_stream_read_failed", "oversize": "model_stream_frame_too_large", "canceled": "model_stream_canceled"}
			if result.ErrorCode != codes[ending] || (ending == "completed") != (result.Disposition == core.SendCompleted) {
				t.Fatalf("wrong stream classification: %+v", result)
			}
		})
	}
}

func TestRetainedSendObservationsAreBoundedOriginalMessages(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	permission := core.PermissionAllowed
	makeObservation := func(index int) core.QuotaObservation {
		return core.QuotaObservation{AccountID: "a", Generation: "g", ReceivedAt: at.Add(time.Duration(index) * time.Second), Source: "original"}
	}
	first := makeObservation(0)
	first.FiveHour = &core.WindowEvidence{State: core.WindowActive}
	second := makeObservation(1)
	second.Weekly = &core.WindowEvidence{State: core.WindowActive}
	third := makeObservation(2)
	third.Permission = &permission
	var kept []core.QuotaObservation
	for _, o := range []core.QuotaObservation{first, second, third} {
		kept = retainSendObservations(kept, o)
	}
	if len(kept) != 3 || !kept[0].ReceivedAt.Equal(first.ReceivedAt) || kept[0].Permission != nil || kept[0].Weekly != nil || kept[1].FiveHour != nil || kept[2].FiveHour != nil || kept[2].Weekly != nil {
		t.Fatalf("original messages merged: %+v", kept)
	}
	for index := 3; index < 1000; index++ {
		o := makeObservation(index)
		o.Permission = &permission
		kept = retainSendObservations(kept, o)
		if len(kept) != 3 || !kept[0].ReceivedAt.Equal(first.ReceivedAt) || !kept[1].ReceivedAt.Equal(second.ReceivedAt) || !kept[2].ReceivedAt.Equal(o.ReceivedAt) {
			t.Fatal("long stream grew storage or refreshed older field timestamps")
		}
	}
	final := makeObservation(1000)
	final.Permission, final.FiveHour, final.Weekly = &permission, first.FiveHour, second.Weekly
	kept = retainSendObservations(kept, final)
	if len(kept) != 1 || !kept[0].ReceivedAt.Equal(final.ReceivedAt) {
		t.Fatal("fully superseded messages were retained")
	}
}

func TestRetainedSendObservationsKeepActivationBeforeUnknown(t *testing.T) {
	at := time.Now().UTC()
	active := core.QuotaObservation{ReceivedAt: at, FiveHour: &core.WindowEvidence{State: core.WindowActive}}
	kept := retainSendObservations(nil, active)
	for index := 1; index <= 1000; index++ {
		unknown := core.QuotaObservation{ReceivedAt: at.Add(time.Duration(index) * time.Second), FiveHour: &core.WindowEvidence{State: core.WindowUnknown}}
		kept = retainSendObservations(kept, unknown)
		if len(kept) != 2 || kept[0].FiveHour.State != core.WindowActive || !kept[0].ReceivedAt.Equal(at) || !kept[1].ReceivedAt.Equal(unknown.ReceivedAt) {
			t.Fatal("later unknown field discarded activation evidence or grew storage")
		}
	}
}
