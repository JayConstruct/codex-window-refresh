package runtime

import (
	"context"
	"errors"
	"testing"

	"codex-window-refresh/internal/core"
)

type generationOnlyHost struct {
	generatingHost
	catalog map[core.AccountID]core.GenerationAccount
}

func (h *generationOnlyHost) GenerationAccounts() map[core.AccountID]core.GenerationAccount {
	copied := map[core.AccountID]core.GenerationAccount{}
	for id, account := range h.catalog {
		copied[id] = account
	}
	return copied
}

func (h *generationOnlyHost) Capabilities(context.Context) (core.HostCapabilities, error) {
	return core.HostCapabilities{StableAccountIdentity: true, ExactAccountExecution: true}, nil
}

type generationFailStore struct{ core.Store }

func (s generationFailStore) Commit(ctx context.Context, rev uint64, state core.CoreState) (uint64, error) {
	if state.GenerationJob != nil && state.GenerationJob.Phase == "dispatched" {
		return 0, errors.New("disk unavailable")
	}
	return s.Store.Commit(ctx, rev, state)
}

func TestAntigravityGenerationStaysOutsideCodexPoolAndNeverAutomaticallyRepeats(t *testing.T) {
	for _, scenario := range []string{"success", "disabled", "binding", "foreground", "disk", "unknown", "invalid_output", "not_sent"} {
		t.Run(scenario, func(t *testing.T) {
			e, base, store, clock := fixture(t, false, true)
			base.initial = []core.HostAccount{}
			calls := 0
			host := &generationOnlyHost{catalog: map[core.AccountID]core.GenerationAccount{"ag": {ID: "ag", Provider: "antigravity", Generation: "ag-generation", Enabled: true}}}
			host.generatingHost = generatingHost{Host: base, generate: func(_ context.Context, request core.ExactRequest, count int) (core.SendResult, []string, error) {
				calls++
				if request.Provider != "antigravity" || request.AccountID != "ag" || request.Generation != "ag-generation" || request.Model != "ag-model" || count != 5 || store.s.GenerationJob.Phase != "dispatched" {
					t.Fatal("generation was not committed with the selected provider/account/model")
				}
				entry := store.s.History[len(store.s.History)-1]
				if entry.Provider != "antigravity" || entry.Status != "dispatched" || entry.OperationID != request.OperationID {
					t.Fatalf("generation history lost provider or dispatch evidence: %+v", entry)
				}
				if scenario == "unknown" {
					return core.SendResult{Disposition: core.SendUnknown}, nil, errors.New("private stream failure")
				}
				if scenario == "not_sent" {
					return core.SendResult{Disposition: core.SendDefinitelyNotSent}, nil, nil
				}
				if scenario == "invalid_output" {
					return core.SendResult{Disposition: core.SendCompleted}, []string{"bad"}, nil
				}
				return core.SendResult{Disposition: core.SendCompleted}, generationCandidates(), nil
			}}
			e.host = host
			if err := e.RefreshAccounts(context.Background()); err != nil {
				t.Fatal(err)
			}
			request := generationRequest()
			request.Provider, request.AccountID, request.Model = "antigravity", "ag", "ag-model"
			if e.QueuePromptGeneration(request) != "queued" || e.QueuePromptGeneration(request) != "already_queued" || e.QueueQuotaQuery("") != "busy" {
				t.Fatal("Antigravity generation did not queue exclusively and idempotently")
			}
			switch scenario {
			case "disabled":
				account := host.catalog["ag"]
				account.Enabled = false
				host.catalog["ag"] = account
			case "binding":
				account := host.catalog["ag"]
				account.Generation = "replacement"
				host.catalog["ag"] = account
			case "foreground":
				e.EnqueueActivity(core.ActivityEvent{RequestID: "foreground", AccountID: "ag", Generation: "ag-generation", Identity: "user", At: clock.now, Phase: core.ActivityStarted})
			case "disk":
				e.store = generationFailStore{Store: store}
			}
			err := e.Step(context.Background())
			if scenario == "disk" {
				if err == nil || calls != 0 {
					t.Fatal("failed dispatch commit allowed generation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			state := e.Snapshot()
			if len(state.Accounts) != 0 || len(state.Reservations) != 0 || len(base.sends) != 0 || base.reads != 0 || len(e.SchedulerDiagnostics().(SchedulingSummary).Plan.NextWakes) != 0 {
				t.Fatal("Antigravity generation entered Codex scheduling, reservations or quota reads")
			}
			expected := "completed"
			switch scenario {
			case "disabled", "binding", "foreground":
				expected = "skipped"
			case "unknown":
				expected = "unknown"
			case "invalid_output", "not_sent":
				expected = "failed"
			}
			if state.GenerationJob.Phase != expected || ((expected == "skipped") != (calls == 0)) {
				t.Fatalf("unexpected generation result: calls=%d job=%+v", calls, state.GenerationJob)
			}
			if scenario == "success" && len(state.GenerationJob.Candidates) != 5 {
				t.Fatal("candidates missing")
			}
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls > 1 || e.cfg.Execution.PromptPool[0] != "Reply OK." {
				t.Fatal("generation repeated or changed saved policy")
			}
			restarted, err := New(e.cfg, host, store, clock, fixedRandom{})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls > 1 {
				t.Fatal("restart retried generation")
			}
			if scenario == "unknown" {
				request.RequestID = "different-generation-id"
				if restarted.QueuePromptGeneration(request) != "delivery_evidence_pending" {
					t.Fatal("restart lost unknown delivery suppression")
				}
			}
		})
	}
}

func TestGenerationProviderCannotSelectAnotherProvidersAccount(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	e.host = generatingHost{Host: e.host}
	for _, provider := range []string{"antigravity", "other"} {
		request := generationRequest()
		request.Provider = provider
		if got := e.QueuePromptGeneration(request); got != "account_not_found" && got != "invalid_generation_parameters" {
			t.Fatalf("provider mismatch queued generation: %s", got)
		}
	}
}
