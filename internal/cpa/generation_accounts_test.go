package cpa

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestAntigravityGenerationPinsProviderAuthAndTranslatedResponses(t *testing.T) {
	for _, scenario := range []string{"success", "disabled", "unavailable", "identity_changed", "provider_changed", "missing_completion"} {
		t.Run(scenario, func(t *testing.T) {
			output, _ := json.Marshal([]string{"Hello.", "Hi.", "Ready.", "Yes.", "OK."})
			terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(output)}}}}}})
			base := &fakeCallbacks{upstream: "codex-upstream", chunks: []streamRead{{Payload: append([]byte("data: "), terminal...), Done: true}}}
			entry := authEntry{ID: "antigravity-account", AuthIndex: "ag-index", Provider: "antigravity", Email: "identity@example.test", ProjectID: "project"}
			callback := callbackFunc(func(ctx context.Context, method string, req, res any) error {
				if method == "host.auth.list" {
					return respond(res, authList{Files: []authEntry{{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex"}, entry}})
				}
				if selected, ok := req.(authRequest); ok && selected.AuthIndex == "ag-index" {
					if method != "host.auth.get_runtime" {
						t.Fatal("generation read raw Antigravity credentials")
					}
					return respond(res, authRuntime{Auth: entry})
				}
				return base.Call(ctx, method, req, res)
			})
			host := NewHost(callback, nil)
			codex, err := host.Accounts(context.Background())
			if err != nil || len(codex) != 1 || codex[0].ID != "host-auth" {
				t.Fatalf("Antigravity entered the Codex quota pool: %+v %v", codex, err)
			}
			catalog := host.GenerationAccounts()
			account := catalog["antigravity-account"]
			if !account.Enabled || account.Provider != "antigravity" || account.Generation == "" {
				t.Fatalf("generation catalog missing account: %+v", catalog)
			}
			raw, _ := json.Marshal(catalog)
			if strings.Contains(string(raw), entry.Email) || strings.Contains(string(raw), "project\"") {
				t.Fatal("catalog exposed raw identity metadata")
			}
			delete(catalog, account.ID)
			if len(host.GenerationAccounts()) != 1 || host.ActivityAccounts()[account.ID].Generation != account.Generation {
				t.Fatal("catalog is mutable or foreground activity cannot bind to the account")
			}
			switch scenario {
			case "disabled":
				entry.Disabled = true
			case "unavailable":
				entry.Unavailable = true
			case "identity_changed":
				entry.Email = "replacement@example.test"
			case "provider_changed":
				entry.Provider = "codex"
			case "missing_completion":
				base.chunks = nil
			}
			request := core.ExactRequest{Provider: "antigravity", AccountID: account.ID, Generation: account.Generation, OperationID: "generate-once", Model: "registered-antigravity-model", Prompt: core.GenerationPrompt(5)}
			result, candidates, err := host.GeneratePrompts(context.Background(), request, 5)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "success" && scenario != "missing_completion" {
				if result.Disposition != core.SendDefinitelyNotSent || base.opened {
					t.Fatalf("invalid credential dispatched: %+v", result)
				}
				return
			}
			if !base.closed || base.captured.AuthID != string(account.ID) || base.captured.ForcedProvider != "antigravity" || base.captured.Model != request.Model || base.captured.EntryProtocol != "openai-response" || base.captured.ExitProtocol != "openai-response" || result.Observation != nil {
				t.Fatalf("wrong generation routing or leaked quota evidence: %+v %+v", base.captured, result)
			}
			if scenario == "success" && (result.Disposition != core.SendCompleted || len(candidates) != 5) {
				t.Fatalf("translated generation failed: %+v %v", result, candidates)
			}
			if scenario == "missing_completion" && result.Disposition != core.SendUnknown {
				t.Fatal("missing completion was reported successful")
			}
			base.opened = false
			if rejected, _ := host.SendExact(context.Background(), request); rejected.Disposition != core.SendDefinitelyNotSent || base.opened {
				t.Fatal("Antigravity was usable for a Codex wake request")
			}
		})
	}
}

func TestAntigravityCatalogIsolatesMissingOrAmbiguousIdentities(t *testing.T) {
	for _, scenario := range []string{"missing_email", "missing_index", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			entry := authEntry{ID: "ag", AuthIndex: "index", Provider: "antigravity", Email: "user@example.test"}
			if scenario == "missing_email" {
				entry.Email = ""
			}
			if scenario == "missing_index" {
				entry.AuthIndex = ""
			}
			callback := callbackFunc(func(_ context.Context, method string, req, res any) error {
				if method == "host.auth.list" {
					entries := []authEntry{entry}
					if scenario == "duplicate" {
						entries = append(entries, entry)
					}
					return respond(res, authList{Files: entries})
				}
				return respond(res, authRuntime{Auth: entry})
			})
			host := NewHost(callback, nil)
			if _, err := host.Accounts(context.Background()); err != nil || len(host.GenerationAccounts()) != 0 {
				t.Fatal("unvalidated Antigravity identity exposed as a generation account")
			}
		})
	}
}
