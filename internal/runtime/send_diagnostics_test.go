package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestManualWakePreservesSafeStreamFailureWithoutRetry(t *testing.T) {
	for _, scenario := range []struct {
		name, code, expected string
		failure              error
	}{
		{name: "nil error with classified stream failure", code: "model_stream_failed", expected: "model_stream_failed"},
		{name: "missing completion", code: "model_stream_missing_completion", expected: "model_stream_missing_completion"},
		{name: "private result classification", code: "Bearer private-token upstream body", expected: "send_failed"},
		{name: "private callback error", failure: errors.New("Bearer private-token upstream body"), expected: "send_failed"},
		{name: "unknown without classified failure", expected: "delivery_unconfirmed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			engine, host, _, clock := manualWakeFixture(t)
			sends := 0
			engine.host = controlledHost{Host: engine.host, send: func(context.Context, core.ExactRequest) (core.SendResult, error) {
				sends++
				return core.SendResult{Disposition: core.SendUnknown, ErrorCode: scenario.code}, scenario.failure
			}}
			_, jobID := engine.QueueManualWakeJob("a")
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			job := findJob(t, engine, jobID)
			if job.Phase != "unknown" || job.ManualWake.LastError != scenario.expected {
				t.Fatalf("stream diagnosis missing or unsafe: %+v", job.ManualWake)
			}
			if engine.QueueManualWake("a") != "delivery_evidence_pending" || engine.Snapshot().Accounts["a"].Task.OperationID == "" {
				t.Fatal("failure diagnosis removed ambiguous-delivery suppression")
			}
			for _, entry := range engine.Snapshot().History {
				if entry.Kind == "wake" {
					expected := scenario.expected
					if expected == "delivery_unconfirmed" {
						expected = ""
					}
					if entry.Status != "unknown" || entry.ErrorCode != expected {
						t.Fatalf("history diagnosis missing or unsafe: %+v", entry)
					}
				}
			}
			clock.now = clock.now.Add(time.Second)
			observation := host.observation("a", "g", true)
			observation.Class = core.ObservationExecution
			engine.EnqueueObservation(observation)
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			job = findJob(t, engine, jobID)
			legacy := engine.SchedulerDiagnostics().(SchedulingSummary).ManualWake
			if job.Phase != "completed" || job.ManualWake.LastError != "" || legacy.Phase != "completed" || legacy.LastError != "" || sends != 1 {
				t.Fatal("later activation proof did not clear job error or sent a duplicate request")
			}
		})
	}
}

func TestImmediateWakeActivationOverridesStreamFailureInJobStatus(t *testing.T) {
	engine, host, _, _ := manualWakeFixture(t)
	engine.host = controlledHost{Host: engine.host, send: func(ctx context.Context, request core.ExactRequest) (core.SendResult, error) {
		result, err := host.SendExact(ctx, request)
		result.Disposition, result.ErrorCode = core.SendUnknown, "model_stream_read_failed"
		return result, err
	}}
	_, jobID := engine.QueueManualWakeJob("a")
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, engine, jobID)
	legacy := engine.SchedulerDiagnostics().(SchedulingSummary).ManualWake
	if job.Phase != "completed" || job.ManualWake.LastError != "" || legacy.Phase != "completed" || legacy.LastError != "" || len(host.sends) != 1 {
		t.Fatalf("positive activation proof was hidden by a stream error: %+v %+v", job.ManualWake, legacy)
	}
}
