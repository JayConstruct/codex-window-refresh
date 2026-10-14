package cpa

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestSelectedAccountLookupOnlyRevalidatesSelectedCredential(t *testing.T) {
	calls := map[string]int{}
	upstream, disabled := "upstream-42", false
	callback := callbackFunc(func(_ context.Context, method string, request, response any) error {
		calls[method]++
		if method == "host.auth.list" {
			entries := []authEntry{}
			for i := 0; i < 100; i++ {
				entries = append(entries, authEntry{ID: fmt.Sprintf("account-%d", i), AuthIndex: fmt.Sprintf("index-%d", i), Provider: "codex"})
			}
			return respond(response, authList{Files: entries})
		}
		index := request.(authRequest).AuthIndex
		var n int
		_, _ = fmt.Sscanf(index, "index-%d", &n)
		if method == "host.auth.get_runtime" {
			return respond(response, authRuntime{Auth: authEntry{ID: fmt.Sprintf("account-%d", n), AuthIndex: index, Provider: "codex", Disabled: n == 42 && disabled}})
		}
		if method == "host.auth.get" {
			id := fmt.Sprintf("upstream-%d", n)
			if n == 42 {
				id = upstream
			}
			return respond(response, authGet{AuthIndex: index, JSON: []byte(fmt.Sprintf(`{"type":"codex","account_id":%q}`, id))})
		}
		return errors.New("unexpected callback")
	})
	h := NewHost(callback, nil)
	if _, err := h.Accounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls = map[string]int{}
	first, err := h.Account(context.Background(), "account-42")
	if err != nil || !first.Enabled || len(calls) != 2 || calls["host.auth.get_runtime"] != 1 || calls["host.auth.get"] != 1 {
		t.Fatalf("selected lookup scanned other credentials: %+v %+v %v", first, calls, err)
	}
	h.SetAccountBaseline(core.AccountState{ID: first.ID, Generation: first.Generation})
	upstream, disabled = "replacement", true
	second, err := h.Account(context.Background(), "account-42")
	if err != nil || second.Enabled || second.Generation == first.Generation || h.ActivityAccounts()[second.ID] != second {
		t.Fatalf("selected identity/status was not refreshed: %+v %v", second, err)
	}
	if _, ok := h.baselines[first.ID]; ok {
		t.Fatal("selected identity replacement retained its old baseline")
	}
	before := len(calls)
	if _, err := h.Account(context.Background(), "unknown"); !errors.Is(err, ErrBindingUnavailable) || len(calls) != before {
		t.Fatal("unknown lookup guessed an identity")
	}
}

func TestSelectedAccountInvalidationDoesNotRetainBinding(t *testing.T) {
	for _, failure := range []string{"removed", "wrong index", "credential", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			base := &fakeCallbacks{upstream: "original"}
			fail := false
			calls := 0
			callback := callbackFunc(func(ctx context.Context, method string, req, out any) error {
				calls++
				if fail {
					switch {
					case failure == "removed" && method == "host.auth.get_runtime":
						return respond(out, authRuntime{})
					case failure == "wrong index" && method == "host.auth.get_runtime":
						return respond(out, authRuntime{Auth: authEntry{ID: "host-auth", AuthIndex: "wrong", Provider: "codex"}})
					case failure == "credential" && method == "host.auth.get":
						return errors.New("private credential failure")
					case failure == "cancel" && method == "host.auth.get":
						return context.DeadlineExceeded
					}
				}
				return base.Call(ctx, method, req, out)
			})
			h := NewHost(callback, nil)
			accounts, err := h.Accounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			old := accounts[0]
			h.SetAccountBaseline(core.AccountState{ID: old.ID, Generation: old.Generation})
			fail = true
			a, err := h.Account(context.Background(), old.ID)
			if failure == "cancel" {
				if !errors.Is(err, context.DeadlineExceeded) || h.ActivityAccounts()[old.ID] != old || h.baselines[old.ID].Generation != old.Generation {
					t.Fatal("context failure published a partial identity")
				}
				return
			}
			if err != nil || a.Enabled || a.Generation == old.Generation || a.Generation == "" {
				t.Fatalf("invalid binding retained: %+v %v", a, err)
			}
			if _, ok := h.ActivityAccounts()[old.ID]; ok || len(h.AccountInventoryDiagnostics()) != 1 {
				t.Fatal("invalid binding remained usable")
			}
			if _, ok := h.baselines[old.ID]; ok {
				t.Fatal("invalidated account retained its old baseline")
			}
			before := calls
			if again, err := h.Account(context.Background(), old.ID); err != nil || again != a || calls != before {
				t.Fatal("unvalidated account was guessed or repeatedly queried")
			}
			result, err := h.SendExact(context.Background(), core.ExactRequest{AccountID: old.ID, Generation: old.Generation, Model: "m", Prompt: "test"})
			if err != nil || result.Disposition != core.SendDefinitelyNotSent || base.opened {
				t.Fatal("invalidated identity reached model execution")
			}
		})
	}
}

func TestSelectedAccountDoesNotRestoreConcurrentlyRemovedInventory(t *testing.T) {
	base := &fakeCallbacks{upstream: "original"}
	entered, resume := make(chan struct{}), make(chan struct{})
	callback := callbackFunc(func(ctx context.Context, method string, req, out any) error {
		if method == "host.auth.get_runtime" {
			close(entered)
			<-resume
		}
		return base.Call(ctx, method, req, out)
	})
	h := NewHost(callback, nil)
	id := core.AccountID("host-auth")
	h.bindings[id] = AuthBinding{AccountID: id, AuthIndex: "auth-index", UpstreamAccountID: "original", Generation: generation(string(id), "original")}
	h.accounts[id] = core.HostAccount{ID: id, Generation: h.bindings[id].Generation, Enabled: true}
	done := make(chan error, 1)
	go func() { _, err := h.Account(context.Background(), id); done <- err }()
	<-entered
	h.mu.Lock()
	delete(h.bindings, id)
	delete(h.accounts, id)
	h.mu.Unlock()
	close(resume)
	if err := <-done; !errors.Is(err, ErrBindingChanged) || len(h.ActivityAccounts()) != 0 {
		t.Fatal("selected lookup restored an obsolete binding")
	}
}

func TestSelectedAccountBaselineRetainsDetachedWindowEvidence(t *testing.T) {
	h := NewHost(nil, nil)
	other := core.AccountState{ID: "other", Generation: "other-generation"}
	h.SetAccountBaseline(other)
	account := core.AccountState{
		ID: "selected", Generation: "selected-generation", Task: &core.MergedTask{},
		FiveHour: core.WindowEvidence{Remaining: &core.QuotaAmount{Value: 80}},
		Weekly:   core.WindowEvidence{Remaining: &core.QuotaAmount{Value: 90}},
	}
	h.SetAccountBaseline(account)
	account.FiveHour.Remaining.Value = 0
	account.Weekly.Remaining.Value = 0
	stored := h.baselines[account.ID]
	if stored.Task != nil || stored.FiveHour.Remaining.Value != 80 || stored.Weekly.Remaining.Value != 90 || h.baselines[other.ID].Generation != other.Generation {
		t.Fatal("selected baseline retained mutable evidence or replaced other accounts")
	}
}

func TestInventoryPrunesRemovedInvalidAndReplacedAccountBaselines(t *testing.T) {
	base := &fakeCallbacks{upstream: "original"}
	removed, invalid := false, false
	callback := callbackFunc(func(ctx context.Context, method string, req, out any) error {
		if removed && method == "host.auth.list" {
			return respond(out, authList{})
		}
		if invalid && method == "host.auth.get" {
			return errors.New("unreadable credential")
		}
		return base.Call(ctx, method, req, out)
	})
	h := NewHost(callback, nil)
	for _, scenario := range []string{"unchanged", "replaced", "invalid", "removed"} {
		removed, invalid, base.upstream = false, false, "original"
		accounts, err := h.Accounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		account := accounts[0]
		h.SetAccountBaseline(core.AccountState{ID: account.ID, Generation: account.Generation})
		// Old single-account baselines must not accumulate across inventories.
		h.SetAccountBaseline(core.AccountState{ID: "old-removed", Generation: "old-generation"})
		switch scenario {
		case "replaced":
			base.upstream = "replacement"
		case "invalid":
			invalid = true
		case "removed":
			removed = true
		}
		if _, err := h.Accounts(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := 0
		if scenario == "unchanged" {
			want = 1
			if h.baselines[account.ID].Generation != account.Generation {
				t.Fatal("unchanged inventory discarded valid baseline")
			}
		}
		if len(h.baselines) != want {
			t.Fatalf("%s inventory retained stale baselines: %+v", scenario, h.baselines)
		}
	}
}
