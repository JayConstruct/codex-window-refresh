package runtime

import (
	"context"
	"errors"
	"testing"

	"codex-window-refresh/internal/core"
)

type generatingHost struct {
	core.Host
	generate func(context.Context, core.ExactRequest, int) (core.SendResult, []string, error)
}

func (host generatingHost) GeneratePrompts(ctx context.Context, request core.ExactRequest, count int) (core.SendResult, []string, error) {
	return host.generate(ctx, request, count)
}

func generationRequest() core.PromptGenerationRequest {
	return core.PromptGenerationRequest{RequestID: "test-generation-001", AccountID: "a", Model: "generator-model", Count: 5, Target: "default"}
}
func generationCandidates() []string {
	return []string{"Hello.", "Say hi.", "Reply ready.", "Reply yes.", "A short greeting."}
}

func TestGenerationRunsOnceWithinActiveWindowAndDoesNotSavePolicy(t *testing.T) {
	engine, host, store, _ := fixture(t, false, false)
	engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
	host.active = true
	before := copyConfig(engine.cfg)
	calls := 0
	engine.host = generatingHost{Host: host, generate: func(_ context.Context, request core.ExactRequest, count int) (core.SendResult, []string, error) {
		calls++
		if !store.dispatchSeen || store.s.GenerationJob == nil || store.s.GenerationJob.Phase != "dispatched" || request.Model != "generator-model" || count != 5 || request.Prompt != core.GenerationPrompt(5) {
			t.Fatal("generation not bound and committed before network")
		}
		if len(store.s.History) == 0 || store.s.History[len(store.s.History)-1].OperationID != request.OperationID {
			t.Fatal("generation history missing at send boundary")
		}
		return core.SendResult{Disposition: core.SendCompleted}, generationCandidates(), nil
	}}
	request := generationRequest()
	if engine.QueuePromptGeneration(request) != "queued" || engine.QueuePromptGeneration(request) != "already_queued" || engine.QueueManualWake("a") != "busy" || engine.QueueQuotaQuery("") != "busy" {
		t.Fatal("generation queue failed to isolate or coalesce requests")
	}
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := engine.Snapshot().GenerationJob
	if calls != 1 || job.Phase != "completed" || len(job.Candidates) != 5 || engine.state.Accounts["a"].Task.OperationID != "" {
		t.Fatalf("generation result did not finish: %+v", job)
	}
	if len(engine.state.Activity) != 0 || engine.cfg.Execution.PromptPool[0] != before.Execution.PromptPool[0] {
		t.Fatal("generation changed user activity or saved policy")
	}
	job.Candidates[0] = "mutated"
	if engine.Snapshot().GenerationJob.Candidates[0] == "mutated" {
		t.Fatal("generation status leaked mutable state")
	}
	if engine.QueuePromptGeneration(request) != "already_queued" {
		t.Fatal("completed idempotency key triggered another request")
	}
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("generation auto repeated")
	}
}

func TestGenerationUnknownKeepsDeliverySuppressionAcrossRestart(t *testing.T) {
	engine, host, store, clock := fixture(t, false, false)
	engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
	host.active = true
	calls := 0
	generator := generatingHost{Host: host, generate: func(context.Context, core.ExactRequest, int) (core.SendResult, []string, error) {
		calls++
		return core.SendResult{Disposition: core.SendUnknown}, nil, errors.New("private failure")
	}}
	engine.host = generator
	request := generationRequest()
	engine.QueuePromptGeneration(request)
	if err := engine.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.state.Accounts["a"].Task.OperationID == "" || engine.state.GenerationJob.Phase != "unknown" {
		t.Fatal("old active preflight evidence cleared unknown generation suppression")
	}
	restarted, err := New(engine.cfg, generator, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	request.RequestID = "test-generation-002"
	if restarted.QueuePromptGeneration(request) != "delivery_evidence_pending" {
		t.Fatal("restart allowed ambiguous generation resend")
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("restart retried content generation")
	}
}

func TestGenerationFailuresNeverRetryOrOverwritePools(t *testing.T) {
	for _, scenario := range []string{"invalid_output", "not_sent", "disk", "disabled", "foreground", "binding"} {
		t.Run(scenario, func(t *testing.T) {
			engine, host, store, clock := fixture(t, false, false)
			engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
			calls := 0
			engine.host = generatingHost{Host: host, generate: func(context.Context, core.ExactRequest, int) (core.SendResult, []string, error) {
				calls++
				if scenario == "not_sent" {
					return core.SendResult{Disposition: core.SendDefinitelyNotSent}, nil, nil
				}
				return core.SendResult{Disposition: core.SendCompleted}, []string{"bad"}, nil
			}}
			engine.QueuePromptGeneration(generationRequest())
			switch scenario {
			case "disk":
				store.failDispatch = true
			case "disabled":
				host.initial[0].Enabled = false
			case "foreground":
				engine.EnqueueActivity(core.ActivityEvent{RequestID: "foreground", AccountID: "a", Generation: "g", Identity: "real", At: clock.now, Phase: core.ActivityStarted})
			case "binding":
				host.initial[0].Generation = "changed"
			}
			err := engine.Step(context.Background())
			if scenario == "disk" {
				if err == nil || calls != 0 {
					t.Fatal("disk failure did not prevent generation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "invalid_output" || scenario == "not_sent" {
				if calls != 1 || engine.state.GenerationJob.Phase != "failed" {
					t.Fatal("failed generation misclassified")
				}
			} else if calls != 0 || engine.state.GenerationJob.Phase != "skipped" {
				t.Fatal("unsafe generation dispatched")
			}
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls > 1 || engine.cfg.Execution.PromptPool[0] != "Reply OK." {
				t.Fatal("failure retried or changed pool")
			}
		})
	}
}
