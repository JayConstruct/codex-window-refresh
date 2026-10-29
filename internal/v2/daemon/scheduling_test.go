package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
	"codex-window-refresh/internal/v2/store"
)

type earnedResetSource struct {
	consumed     []string
	loseResponse bool
	reads        int
}

func (s *earnedResetSource) Accounts(context.Context) ([]store.Account, error) {
	return nil, errors.New("unexpected inventory request")
}
func (s *earnedResetSource) Query(_ context.Context, a store.Account) (json.RawMessage, error) {
	at := time.Now().UTC()
	allowed := core.PermissionAllowed
	remaining := 10.0
	if len(s.consumed) > 0 {
		remaining = 100
	}
	w := func(kind core.WindowKind) *core.WindowEvidence {
		return &core.WindowEvidence{Kind: kind, State: core.WindowActive, Remaining: &core.QuotaAmount{Value: remaining, Unit: "percent"}, ResetAt: at.Add(time.Hour)}
	}
	return json.Marshal(quota.Snapshot{Binding: a.Binding, Source: "fixture", ObservedAt: at, ReadStartedAt: at, Permission: &allowed, FiveHour: w(core.FiveHour), Weekly: w(core.Weekly)})
}
func (s *earnedResetSource) ReadResetCredits(context.Context, store.Account) (*policy.ResetCredits, error) {
	s.reads++
	expiry := time.Now().Add(time.Hour).Unix()
	return &policy.ResetCredits{AvailableCount: 1, Credits: []policy.ResetCredit{{ID: "card-one", Status: "available", ResetType: "codexRateLimits", ExpiresAt: &expiry}}}, nil
}
func (s *earnedResetSource) ConsumeResetCredit(_ context.Context, _ store.Account, _ string, key string) (string, error) {
	s.consumed = append(s.consumed, key)
	if s.loseResponse {
		s.loseResponse = false
		return "", errors.New("reset_transport_failed")
	}
	return "alreadyRedeemed", nil
}
func TestResetLostResponseRecoversSameAttemptAndVerifiesWithoutModelExecution(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := &earnedResetSource{loseResponse: true}
	service, err := New(st, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{ID: "a", AuthIndex: "i", Binding: "binding", Status: "available"}
	if err = st.SyncInventory(ctx, []store.Account{a}); err != nil {
		t.Fatal(err)
	}
	config := policy.DefaultScheduling()
	config.Cards.Enabled = true
	config.Cards.AutoConsume = true
	if _, err = st.SaveScheduling(ctx, 0, config); err != nil {
		t.Fatal(err)
	}
	if err = service.resetStep(ctx, source); err == nil || len(source.consumed) != 1 {
		t.Fatal("fixture did not lose redemption response", err, source.consumed)
	}
	_, attempts, err := st.ResetViews(ctx)
	if err != nil || len(attempts) != 1 || attempts[0].Phase != "unknown" {
		t.Fatal(attempts, err)
	}
	job, err := st.PendingReset(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.consumeReset(ctx, source, job, 900); err != nil {
		t.Fatal(err)
	}
	if len(source.consumed) != 2 || source.consumed[0] != source.consumed[1] {
		t.Fatal("redemption identity changed", source.consumed)
	}
	_, attempts, err = st.ResetViews(ctx)
	if err != nil || attempts[0].Phase != "completed" {
		t.Fatal(attempts, err)
	}
}
func TestSchedulingAPIHasDefaultsCASAndReadOnlyViews(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := &dashboardSource{}
	s, err := New(st, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/scheduling", "/api/v2/reset-cards"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	c := policy.DefaultScheduling()
	c.Enabled = true
	raw, _ := json.Marshal(map[string]any{"revision": 0, "config": c})
	for i, want := range []int{200, 409} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v2/scheduling", strings.NewReader(string(raw))))
		if w.Code != want {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if source.queryCalls.Load() != 0 || source.accountCalls.Load() != 0 {
		t.Fatal("settings or views performed upstream work")
	}
}
