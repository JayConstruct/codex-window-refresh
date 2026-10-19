package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type wakeCredentialHost struct {
	*wakeHost
	credential string
	err        error
}

func (host *wakeCredentialHost) Call(ctx context.Context, method string, request, response any) error {
	if method == "host.auth.get" {
		if host.err != nil {
			return host.err
		}
		return json.Unmarshal([]byte(host.credential), response)
	}
	return host.wakeHost.Call(ctx, method, request, response)
}

func TestWakeBindingRequiresVerifiableEvidenceWithoutReadingTokens(t *testing.T) {
	for _, fixture := range []struct{ name, credential, code string }{
		{"malformed", `{"auth_index":"i","json":"private-secret"}`, "account_identity_unavailable"},
		{"missing identity", `{}`, "account_identity_unavailable"},
		{"unverified credential type", `{"auth_index":"i","json":{"type":"other","account_id":"upstream"}}`, "account_identity_unavailable"},
		{"missing upstream", `{"auth_index":"i","json":{"type":"codex"}}`, "wake_binding_missing"},
		{"missing index", `{"json":{"type":"codex","account_id":"upstream"}}`, "wake_binding_missing"},
		{"changed index", `{"auth_index":"other-index","json":{"type":"codex","account_id":"upstream"}}`, "account_binding_changed"},
		{"changed upstream", `{"auth_index":"i","json":{"type":"codex","account_id":"other-upstream"}}`, "account_binding_changed"},
		{"token not required", `{"auth_index":"i","json":{"type":"codex","account_id":"upstream"}}`, ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			cpa.Host = &wakeCredentialHost{wakeHost: &wakeHost{host, true}, credential: fixture.credential}
			response, err := cpa.ExecuteWakeWithResponse(context.Background(), account, "fixture-model", "hello")
			if fixture.code == "" {
				if err != nil || host.executes != 1 {
					t.Fatal("identity check must not require credentials", err, host.executes)
				}
			} else if err == nil || err.Error() != fixture.code || host.executes != 0 || response.ErrorCode != fixture.code {
				t.Fatal("unsafe or unverified identity classification", err, response, host.executes)
			}
		})
	}
	cpa, host, account := wakeFixture()
	cpa.Host = &wakeCredentialHost{wakeHost: &wakeHost{host, true}, err: errors.New("account_disabled")}
	if err := cpa.ExecuteWake(context.Background(), account, "fixture-model", "hello"); err == nil || err.Error() != "account_identity_unavailable" || host.executes != 0 {
		t.Fatal("callback error text was treated as verified status", err, host.executes)
	}
}
