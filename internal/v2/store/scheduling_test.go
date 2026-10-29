package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/quota"
)

func schedulingFixture(t *testing.T, s *Store, at time.Time) Account {
	t.Helper()
	a := Account{ID: "a", AuthIndex: "i", Binding: "bound", Status: "available"}
	ctx := context.Background()
	if err := s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	allowed := core.PermissionAllowed
	w := func(kind core.WindowKind, reset time.Time) *core.WindowEvidence {
		return &core.WindowEvidence{Kind: kind, State: core.WindowActive, ResetAt: reset, Remaining: &core.QuotaAmount{Value: 10, Unit: "percent"}}
	}
	q := quota.Snapshot{Binding: a.Binding, Source: "test", ObservedAt: at, ReadStartedAt: at, Permission: &allowed, FiveHour: w(core.FiveHour, at.Add(time.Hour)), Weekly: w(core.Weekly, at.Add(24*time.Hour))}
	raw, _ := json.Marshal(q)
	if err := s.SaveResetQuota(ctx, a, raw); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestSchedulingDefaultAndQuotaBindingFreshness(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Now().UTC()
	a := schedulingFixture(t, s, at)
	settings, err := s.SchedulingSettings(ctx)
	if err != nil || settings.Config.Enabled || settings.Config.Cards.Enabled || settings.Config.Cards.AutoConsume {
		t.Fatal(settings, err)
	}
	settings.Config.Enabled = true
	if _, err = s.SaveScheduling(ctx, settings.Revision, settings.Config); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RoutingSnapshot(ctx, at)
	if err != nil || len(snapshot.Accounts) != 1 || snapshot.Accounts[0].ResetAt != at.Add(time.Hour).Unix() {
		t.Fatal(snapshot, err)
	}
	if _, err = s.SaveScheduling(ctx, settings.Revision, settings.Config); !errors.Is(err, ErrSettingsConflict) {
		t.Fatal("lost configuration CAS", err)
	}
	snapshot, err = s.RoutingSnapshot(ctx, at.Add(6*time.Minute))
	if err != nil || len(snapshot.Accounts) != 0 || len(snapshot.Identities) != 1 || snapshot.Identities[0].Binding != a.Binding || len(snapshot.Exhausted) != 0 {
		t.Fatal("stale evidence selected", snapshot, err)
	}
	a.Binding = "replacement"
	if err = s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.RoutingSnapshot(ctx, at)
	if err != nil || len(snapshot.Accounts) != 0 || len(snapshot.Identities) != 1 || snapshot.Identities[0].Binding != "replacement" {
		t.Fatal("replacement inherited old quota", snapshot, err)
	}
}

func TestSchedulingReadsOldSettingsAndPersistsExplicitStickyOptions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	raw, _ := json.Marshal(policy.DefaultScheduling())
	var old map[string]any
	json.Unmarshal(raw, &old)
	delete(old, "subagent_affinity")
	delete(old, "rebind_on")
	raw, _ = json.Marshal(old)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO scheduling_settings VALUES(1,7,?)", string(raw)); err != nil {
		t.Fatal(err)
	}
	settings, err := s.SchedulingSettings(ctx)
	if err != nil || settings.Revision != 7 || !settings.Config.SubagentAffinity || settings.Config.RebindOn != "quota" {
		t.Fatal(settings, err)
	}
	settings.Config.SubagentAffinity, settings.Config.RebindOn = false, "any"
	if _, err = s.SaveScheduling(ctx, 7, settings.Config); err != nil {
		t.Fatal(err)
	}
	settings, err = s.SchedulingSettings(ctx)
	if err != nil || settings.Revision != 8 || settings.Config.SubagentAffinity || settings.Config.RebindOn != "any" {
		t.Fatal(settings, err)
	}
}

func TestRoutingExhaustionNeedsFreshMatchingEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Now().UTC()
	a := schedulingFixture(t, s, at)
	raw, err := quota.Normalize([]byte(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":`+fmt.Sprint(at.Add(time.Hour).Unix())+`}}}`), nil, a.Binding, "cpa_wham_usage", at.Add(time.Second), at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(raw)
	if err = s.SaveResetQuota(ctx, a, data); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RoutingSnapshot(ctx, at.Add(2*time.Second))
	if err != nil || len(snapshot.Exhausted) != 1 || snapshot.Exhausted[0] != "a" || len(snapshot.Identities) != 1 {
		t.Fatal(snapshot, err)
	}
	snapshot, err = s.RoutingSnapshot(ctx, at.Add(6*time.Minute))
	if err != nil || len(snapshot.Exhausted) != 0 || len(snapshot.Identities) != 1 {
		t.Fatal("stale exhaustion invalidated affinity", snapshot, err)
	}
	a.Binding = "replacement"
	if err = s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.RoutingSnapshot(ctx, at)
	if err != nil || len(snapshot.Exhausted) != 0 {
		t.Fatal("replacement inherited exhaustion", snapshot, err)
	}
}
func TestResetOwnershipSurvivesRestartAndReusesRedemptionKey(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Now().UTC()
	a := schedulingFixture(t, s, at)
	c := policy.DefaultScheduling()
	c.Cards.Enabled = true
	c.Cards.AutoConsume = true
	if _, err = s.SaveScheduling(ctx, 0, c); err != nil {
		t.Fatal(err)
	}
	expires := at.Add(time.Hour).Unix()
	credit := policy.ResetCredit{ID: "card-1", ResetType: "codexRateLimits", Status: "available", ExpiresAt: &expires}
	job, err := s.ClaimReset(ctx, a, credit, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimReset(ctx, a, credit, at); !errors.Is(err, ErrNotFound) {
		t.Fatal("concurrent reset admitted", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, guardErr := generationAccount(ctx, tx, a.ID)
	tx.Rollback()
	if !errors.Is(guardErr, ErrGenerationAccount) {
		t.Fatal("generation overlapped reset", guardErr)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.PendingReset(ctx, at.Add(time.Second))
	if err != nil || recovered.ID != job.ID || recovered.CreditID != credit.ID {
		t.Fatal("redemption key changed", recovered, err)
	}
	if err = s.FinishReset(ctx, recovered, "completed", "alreadyRedeemed", "", at); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimReset(ctx, a, credit, at.Add(16*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatal("used credit admitted", err)
	}
}

func TestResetDoesNotSpendCardsOnUnknownStaleOrSoonNaturallyResettingWindows(t *testing.T) {
	at := time.Now().UTC()
	expiry := at.Add(time.Hour).Unix()
	c := policy.ResetCredit{ID: "card", Status: "available", ResetType: "codexRateLimits", ExpiresAt: &expiry}
	p := policy.DefaultScheduling().Cards
	makeQuota := func() quota.Snapshot {
		return quota.Snapshot{ObservedAt: at, FiveHour: &core.WindowEvidence{State: core.WindowActive, ResetAt: at.Add(time.Hour), Remaining: &core.QuotaAmount{Value: 5, Unit: "percent"}}}
	}
	if !resetWorthwhile(makeQuota(), c, p, at) {
		t.Fatal("useful expiring card was skipped")
	}
	unknown := c
	unknown.ExpiresAt = nil
	if resetWorthwhile(makeQuota(), unknown, p, at) {
		t.Fatal("guessed unknown card expiration")
	}
	q := makeQuota()
	q.Fields = map[string]quota.Stamp{"five_hour": {ObservedAt: at.Add(-2 * time.Minute)}}
	if resetWorthwhile(q, c, p, at) {
		t.Fatal("fresh global timestamp hid stale window evidence")
	}
	q = makeQuota()
	q.FiveHour.ResetAt = at.Add(time.Minute)
	if resetWorthwhile(q, c, p, at) {
		t.Fatal("spent card just before natural reset")
	}
	q = makeQuota()
	q.FiveHour.Remaining.Value = 100
	if resetWorthwhile(q, c, p, at) {
		t.Fatal("spent card on unused window")
	}
}

func TestResetViewsUseQuotaSummaryWithoutRefreshingDetailFreshness(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Now().UTC()
	a := schedulingFixture(t, s, at)
	update := func(count int, stamp time.Time) {
		t.Helper()
		raw, err := quota.Normalize([]byte(fmt.Sprintf(`{"rate_limit":{"allowed":true},"rate_limit_reset_credits":{"available_count":%d}}`, count)), nil, a.Binding, "cpa_wham_usage", stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(raw)
		if err = s.SaveResetQuota(ctx, a, data); err != nil {
			t.Fatal(err)
		}
	}
	update(2, at.Add(time.Second))
	views, _, err := s.ResetViews(ctx)
	if err != nil || len(views) != 1 || views[0].Credits.AvailableCount != 2 || views[0].Credits.Credits != nil {
		t.Fatal(views, err)
	}
	expires := at.Add(time.Hour).Unix()
	cards := &policy.ResetCredits{AvailableCount: 2, Credits: []policy.ResetCredit{{ID: "known-card", Status: "available", ResetType: "codexRateLimits", ExpiresAt: &expires}}}
	if err = s.RecordResetCredits(ctx, a, cards, "", at.Add(2*time.Second), 900); err != nil {
		t.Fatal(err)
	}
	update(2, at.Add(3*time.Second))
	views, _, err = s.ResetViews(ctx)
	if err != nil || len(views[0].Credits.Credits) != 1 || views[0].DetailsObservedAt != queryStamp(at.Add(2*time.Second)) {
		t.Fatal("summary refreshed detail age", views, err)
	}
	update(1, at.Add(4*time.Second))
	views, _, err = s.ResetViews(ctx)
	if err != nil || views[0].Credits.AvailableCount != 1 || views[0].Credits.Credits != nil {
		t.Fatal("count change kept stale detail", views, err)
	}
	update(0, at.Add(5*time.Second))
	views, _, err = s.ResetViews(ctx)
	if err != nil || views[0].Credits.AvailableCount != 0 || len(views[0].Credits.Credits) != 0 {
		t.Fatal(views, err)
	}
	a.Binding = "replacement"
	if err = s.SyncInventory(ctx, []Account{a}); err != nil {
		t.Fatal(err)
	}
	views, _, err = s.ResetViews(ctx)
	if err != nil || len(views) != 0 {
		t.Fatal("replacement inherited count", views, err)
	}
}
