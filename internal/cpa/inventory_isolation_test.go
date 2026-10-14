package cpa

import (
	"context"
	"errors"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestInventoryIsolatesInvalidCredentialWithoutWeakeningExactBinding(t *testing.T) {
	for _, scenario := range []struct {
		name                                                                       string
		entries                                                                    []authEntry
		runtimeOnly, runtimeMismatch, runtimeFailure, credentialFailure, malformed bool
		code                                                                       string
	}{
		{name: "runtime only", runtimeOnly: true, code: "account_identity_unavailable"},
		{name: "list runtime only", entries: []authEntry{{ID: "bad", AuthIndex: "bad-index", Provider: "codex", RuntimeOnly: true}}, code: "account_identity_unavailable"},
		{name: "missing index", entries: []authEntry{{ID: "bad", Provider: "codex"}}, code: "account_identity_unavailable"},
		{name: "missing ID", entries: []authEntry{{AuthIndex: "bad-index", Provider: "codex"}}, code: "account_identity_unavailable"},
		{name: "duplicate ID", entries: []authEntry{{ID: "bad", AuthIndex: "bad-index", Provider: "codex"}, {ID: "bad", AuthIndex: "other-index", Provider: "codex"}}, code: "account_identity_ambiguous"},
		{name: "runtime identity changed", runtimeMismatch: true, code: "account_binding_changed"},
		{name: "runtime callback failed", runtimeFailure: true, code: "host_callback_failed"},
		{name: "credential callback failed", credentialFailure: true, code: "account_identity_unavailable"},
		{name: "malformed metadata", malformed: true, code: "account_identity_unavailable"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			base := &fakeCallbacks{upstream: "healthy-upstream"}
			badEntries := scenario.entries
			if badEntries == nil {
				badEntries = []authEntry{{ID: "bad", AuthIndex: "bad-index", Provider: "codex"}}
			}
			callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
				if method == "host.auth.list" {
					entries := append([]authEntry{}, badEntries...)
					entries = append(entries, authEntry{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex"})
					return respond(response, authList{Files: entries})
				}
				if req, ok := request.(authRequest); ok && req.AuthIndex == "bad-index" {
					switch method {
					case "host.auth.get_runtime":
						if scenario.runtimeFailure {
							return errors.New("private callback response")
						}
						id := "bad"
						if scenario.runtimeMismatch {
							id = "changed"
						}
						return respond(response, authRuntime{Auth: authEntry{ID: id, AuthIndex: "bad-index", Provider: "codex", RuntimeOnly: scenario.runtimeOnly}})
					case "host.auth.get":
						if scenario.credentialFailure {
							return errors.New("private credential response")
						}
						if scenario.malformed {
							return respond(response, map[string]any{"auth_index": "bad-index", "json": map[string]string{"type": "codex", "access_token": "private"}})
						}
						return respond(response, map[string]any{"auth_index": "bad-index", "json": map[string]string{"type": "codex", "account_id": "bad-upstream"}})
					}
				}
				return base.Call(ctx, method, request, response)
			})
			reads := 0
			host := NewHost(callback, quotaReadFunc(func(_ context.Context, binding AuthBinding) ([]byte, error) {
				reads++
				if binding.AccountID != "host-auth" || binding.UpstreamAccountID != "healthy-upstream" {
					t.Fatal("invalid binding reached reader")
				}
				return []byte(`{"rate_limit":{"allowed":true}}`), nil
			}))
			accounts, err := host.Accounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			byID := map[core.AccountID]core.HostAccount{}
			for _, account := range accounts {
				byID[account.ID] = account
			}
			healthy, ok := byID["host-auth"]
			if !ok || !healthy.Enabled {
				t.Fatalf("healthy account blocked: %+v", accounts)
			}
			if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: healthy.ID, Generation: healthy.Generation}); err != nil || reads != 1 {
				t.Fatalf("healthy query failed: %v", err)
			}
			badID := core.AccountID("bad")
			if scenario.name == "missing ID" {
				badID = ""
				if len(accounts) != 1 {
					t.Fatalf("missing ID became account: %+v", accounts)
				}
			} else {
				bad, ok := byID[badID]
				if !ok || bad.Enabled || bad.Generation == "" || len(accounts) != 2 {
					t.Fatalf("invalid account not safely represented: %+v", accounts)
				}
				if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: badID, Generation: bad.Generation}); !errors.Is(err, ErrBindingUnavailable) || reads != 1 {
					t.Fatalf("invalid credential queried: %v", err)
				}
				result, err := host.SendExact(context.Background(), core.ExactRequest{AccountID: badID, Generation: bad.Generation, Model: "m", Prompt: "test"})
				if err != nil || result.Disposition != core.SendDefinitelyNotSent || base.opened {
					t.Fatalf("invalid credential sent: %+v %v", result, err)
				}
			}
			diagnostics := host.AccountInventoryDiagnostics()
			if len(diagnostics) != 1 || diagnostics[badID] != scenario.code {
				t.Fatalf("unsafe or missing diagnostics: %+v", diagnostics)
			}
			diagnostics[badID] = "modified"
			if host.AccountInventoryDiagnostics()[badID] != scenario.code {
				t.Fatal("diagnostics not copied")
			}
			if _, ok := host.ActivityAccounts()[badID]; ok {
				t.Fatal("invalid credential accepted for activity attribution")
			}
		})
	}
}

func TestInventoryInvalidationAndRecoveryNeverReuseStaleBinding(t *testing.T) {
	base := &fakeCallbacks{upstream: "original"}
	invalid, removed := false, false
	callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
		if method == "host.auth.list" && removed {
			return respond(response, authList{})
		}
		if method == "host.auth.get" && invalid {
			return errors.New("private credential error")
		}
		return base.Call(ctx, method, request, response)
	})
	reads := 0
	host := NewHost(callback, quotaReadFunc(func(context.Context, AuthBinding) ([]byte, error) {
		reads++
		return []byte(`{"rate_limit":{"allowed":true}}`), nil
	}))
	accounts, err := host.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := accounts[0]
	invalid = true
	accounts, err = host.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Enabled || accounts[0].Generation == old.Generation {
		t.Fatalf("invalid identity retained: %+v %v", accounts, err)
	}
	placeholder := accounts[0].Generation
	for _, gen := range []core.CredentialGeneration{old.Generation, placeholder} {
		if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: old.ID, Generation: gen}); !errors.Is(err, ErrBindingUnavailable) || reads != 0 {
			t.Fatalf("cached identity reused: %v", err)
		}
	}
	accounts, err = host.Accounts(context.Background())
	if err != nil || accounts[0].Generation != placeholder {
		t.Fatal("unavailable generation is unstable")
	}
	invalid = false
	base.upstream = "replacement"
	accounts, err = host.Accounts(context.Background())
	if err != nil || !accounts[0].Enabled || accounts[0].Generation == placeholder || accounts[0].Generation == old.Generation || len(host.AccountInventoryDiagnostics()) != 0 {
		t.Fatalf("recovery retained invalid identity: %+v %v", accounts, err)
	}
	if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: old.ID, Generation: old.Generation}); !errors.Is(err, ErrBindingChanged) || reads != 0 {
		t.Fatalf("old generation accepted after recovery: %v", err)
	}
	if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: old.ID, Generation: accounts[0].Generation}); err != nil || reads != 1 {
		t.Fatalf("recovered identity unavailable: %v", err)
	}
	removed = true
	accounts, err = host.Accounts(context.Background())
	if err != nil || len(accounts) != 0 || len(host.AccountInventoryDiagnostics()) != 0 || len(host.ActivityAccounts()) != 0 {
		t.Fatal("removed account retained")
	}
	if _, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: old.ID, Generation: old.Generation}); !errors.Is(err, ErrBindingUnavailable) {
		t.Fatal("removed binding retained")
	}
}

func TestInventoryGlobalFailureOrCancellationDoesNotPublishPartialAccounts(t *testing.T) {
	for _, failure := range []string{"list", "cancel", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			base := &fakeCallbacks{upstream: "original"}
			fail := false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
				if fail {
					if method == "host.auth.list" && failure == "list" {
						return errors.New("private list failure")
					}
					if method == "host.auth.get" {
						if failure == "cancel" {
							cancel()
							return errors.New("private cancellation response")
						}
						if failure == "deadline" {
							return context.DeadlineExceeded
						}
					}
				}
				return base.Call(ctx, method, request, response)
			})
			host := NewHost(callback, nil)
			accounts, err := host.Accounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			old := accounts[0]
			fail = true
			accounts, err = host.Accounts(ctx)
			want := ErrHostCallback
			if failure == "cancel" {
				want = context.Canceled
			}
			if failure == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || accounts != nil {
				t.Fatalf("global failure isolated incorrectly: %+v %v", accounts, err)
			}
			if got := host.ActivityAccounts()[old.ID]; got != old || len(host.AccountInventoryDiagnostics()) != 0 {
				t.Fatalf("partial inventory published: %+v", got)
			}
		})
	}
}
