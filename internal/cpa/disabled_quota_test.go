package cpa

import (
	"context"
	"errors"
	"testing"

	"codex-window-refresh/internal/core"
)

type quotaReadFunc func(context.Context, AuthBinding) ([]byte, error)

func (reader quotaReadFunc) Read(ctx context.Context, binding AuthBinding) ([]byte, error) {
	return reader(ctx, binding)
}

func TestUnavailableAccountsCanReadQuotaButCannotSend(t *testing.T) {
	for _, scenario := range []struct {
		name                  string
		disabled, unavailable bool
		status                string
	}{
		{"disabled", true, false, "active"}, {"unavailable", false, true, "active"}, {"disabled status", false, false, "disabled"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			base := &fakeCallbacks{upstream: "real"}
			callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
				if method == "host.auth.get_runtime" {
					return respond(response, authRuntime{Auth: authEntry{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex", Disabled: scenario.disabled, Unavailable: scenario.unavailable, Status: scenario.status}})
				}
				return base.Call(ctx, method, request, response)
			})
			reads := 0
			host := NewHost(callback, quotaReadFunc(func(_ context.Context, binding AuthBinding) ([]byte, error) {
				reads++
				if binding.AccountID != "host-auth" || binding.UpstreamAccountID != "real" {
					t.Fatal("query lost exact account binding")
				}
				return []byte(`{"rate_limit":{"allowed":false,"limit_reached":true}}`), nil
			}))
			accounts, err := host.Accounts(context.Background())
			if err != nil || len(accounts) != 1 || accounts[0].Enabled {
				t.Fatalf("unavailable account became sendable: %+v %v", accounts, err)
			}
			account := accounts[0]
			wantStatus := core.AccountDisabled
			if scenario.unavailable {
				wantStatus = core.AccountUnavailable
			}
			if account.Availability != wantStatus {
				t.Fatalf("host status lost: %+v", account)
			}
			refreshed, err := host.Account(context.Background(), account.ID)
			if err != nil || refreshed.Availability != wantStatus || refreshed.Enabled {
				t.Fatalf("selected refresh lost host status: %+v %v", refreshed, err)
			}
			observation, err := host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: account.ID, Generation: account.Generation, ReadStartedSeq: 1})
			if err != nil || reads != 1 || observation.Permission == nil || *observation.Permission != core.PermissionBlocked {
				t.Fatalf("read-only quota rejected: %+v %v", observation, err)
			}
			result, err := host.SendExact(context.Background(), core.ExactRequest{AccountID: account.ID, Generation: account.Generation, OperationID: "manual", Model: "model", Prompt: "Reply OK."})
			if err != nil || result.Disposition != core.SendDefinitelyNotSent || base.opened {
				t.Fatalf("quota query lifted model send restrictions: %+v %v", result, err)
			}
		})
	}
}

func TestQuotaReadPreservesCredentialFenceBeforeAndAfterRead(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		base := &fakeCallbacks{upstream: "real"}
		reads := 0
		host := NewHost(base, quotaReadFunc(func(context.Context, AuthBinding) ([]byte, error) {
			reads++
			base.upstream = "replacement"
			return []byte(`{"rate_limit":{"allowed":true}}`), nil
		}))
		accounts, err := host.Accounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !duringRead {
			base.upstream = "replacement"
		}
		account := accounts[0]
		if _, err = host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: account.ID, Generation: account.Generation, ReadStartedSeq: 1}); !errors.Is(err, ErrBindingChanged) {
			t.Fatalf("query accepted changed credentials: %v", err)
		}
		if (duringRead && reads != 1) || (!duringRead && reads != 0) {
			t.Fatal("credential check ran at the wrong boundary")
		}
		if _, err = host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: "removed", Generation: account.Generation, ReadStartedSeq: 2}); !errors.Is(err, ErrBindingUnavailable) {
			t.Fatal("missing account was queried")
		}
	}
}

func TestAccountBecomingUnavailableDuringReadStillReturnsQuota(t *testing.T) {
	base := &fakeCallbacks{upstream: "real"}
	unavailable := false
	callback := callbackFunc(func(ctx context.Context, method string, request, response any) error {
		if method == "host.auth.get_runtime" {
			return respond(response, authRuntime{Auth: authEntry{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex", Unavailable: unavailable}})
		}
		return base.Call(ctx, method, request, response)
	})
	host := NewHost(callback, quotaReadFunc(func(context.Context, AuthBinding) ([]byte, error) {
		unavailable = true
		return []byte(`{"rate_limit":{"allowed":false,"limit_reached":true}}`), nil
	}))
	accounts, err := host.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := accounts[0]
	if _, err = host.ObserveQuota(context.Background(), core.QuotaRead{AccountID: account.ID, Generation: account.Generation, ReadStartedSeq: 1}); err != nil {
		t.Fatalf("status change discarded read-only evidence: %v", err)
	}
	if _, err = host.validate(context.Background(), account.ID, account.Generation); !errors.Is(err, ErrBindingUnavailable) {
		t.Fatal("status change did not block model send")
	}
}
