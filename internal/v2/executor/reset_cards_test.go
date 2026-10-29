package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type resetRuntimeHost struct {
	fixtureHost
	runtime string
	clears  int
}

func (h *resetRuntimeHost) Call(ctx context.Context, method string, request, response any) error {
	if method == "host.auth.get_runtime" {
		return json.Unmarshal([]byte(h.runtime), response)
	}
	if method == "host.routing.reset_cooldown" {
		h.clears++
		return nil
	}
	return h.fixtureHost.Call(ctx, method, request, response)
}

func TestResetAccountProtocolIsIsolatedPinnedAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "fake-codex")
	program := `#!/usr/bin/env python3
import json,os,sys
assert 'ephemeral' in sys.argv[-1]
assert not os.path.exists(os.path.join(os.environ['CODEX_HOME'],'auth.json'))
for line in sys.stdin:
 r=json.loads(line)
 if 'id' not in r: continue
 method=r['method'];params=r.get('params',{})
 if method=='initialize':
  assert params['capabilities']['experimentalApi']
  result={}
 elif method=='account/login/start':
  assert params['chatgptAccountId']=='upstream' and params['accessToken']=='fixture-token'
  result={'type':'chatgptAuthTokens'}
 elif method=='account/rateLimits/read':
  result={'rateLimitResetCredits':{'availableCount':1,'credits':[{'id':'card-1','status':'available','resetType':'codexRateLimits','expiresAt':1999999999}]}}
 elif method=='account/rateLimitResetCredit/consume':
  assert params=={'creditId':'card-1','idempotencyKey':'same-attempt'}
  result={'outcome':'alreadyRedeemed'}
 else: raise RuntimeError('unexpected method')
 print(json.dumps({'id':r['id'],'result':result}),flush=True)
`
	if err := os.WriteFile(helper, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	host := &resetRuntimeHost{runtime: `{"auth":{"id":"a","auth_index":"i","provider":"codex","unavailable":true}}`}
	c := &CPA{Host: host, CodexBinary: helper}
	a := store.Account{ID: "a", AuthIndex: "i", Binding: bindingID(store.Account{ID: "a", AuthIndex: "i"}, "upstream"), Status: "available"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cards, err := c.ReadResetCredits(ctx, a)
	if err != nil || cards == nil || cards.AvailableCount != 1 || len(cards.Credits) != 1 {
		t.Fatal(cards, err)
	}
	outcome, err := c.ConsumeResetCredit(ctx, a, "card-1", "same-attempt")
	if err != nil || outcome != "alreadyRedeemed" {
		t.Fatal(outcome, err)
	}
	if err = c.ClearResetCooldown(ctx, a); err != nil || host.clears != 1 {
		t.Fatal("acknowledged reset cannot release CPA cooldown", err, host.clears)
	}
	a.Binding = "replacement"
	if _, err = c.ConsumeResetCredit(ctx, a, "card-1", "same-attempt"); err == nil || !strings.Contains(err.Error(), "binding_changed") {
		t.Fatal("cross-account reset admitted", err)
	}
	host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","disabled":true}}`
	if _, err = c.ConsumeResetCredit(ctx, a, "card-1", "same-attempt"); err == nil || err.Error() != "account_disabled" {
		t.Fatal("manual disable was ignored", err)
	}
}
