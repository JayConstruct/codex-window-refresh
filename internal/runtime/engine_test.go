package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

type fixedRandom struct{ n time.Duration }

func (r fixedRandom) Duration(lo, hi time.Duration) (time.Duration, error) { return lo + r.n, nil }

type memoryStore struct {
	s            core.CoreState
	failDispatch bool
	dispatchSeen bool
}

func (m *memoryStore) Load(context.Context) (core.CoreState, error) { return cloneState(m.s), nil }
func (m *memoryStore) Commit(_ context.Context, rev uint64, s core.CoreState) (uint64, error) {
	if rev != m.s.Revision {
		return 0, errors.New("CAS")
	}
	for _, a := range s.Accounts {
		if a.Task != nil && a.Task.State == core.TaskDispatchIntent {
			m.dispatchSeen = true
			if m.failDispatch {
				return 0, errors.New("disk")
			}
		}
	}
	m.s = cloneState(s)
	m.s.Revision++
	return m.s.Revision, nil
}

type fakeHost struct {
	clock     *testClock
	store     *memoryStore
	initial   []core.HostAccount
	sends     []core.ExactRequest
	active    bool
	unknown   bool
	queryHook func()
	reads     int
}

func (h *fakeHost) Capabilities(context.Context) (core.HostCapabilities, error) {
	return core.HostCapabilities{StableAccountIdentity: true, ExactAccountExecution: true, QuotaAdapterReady: true}, nil
}
func (h *fakeHost) Accounts(context.Context) ([]core.HostAccount, error) { return h.initial, nil }
func (h *fakeHost) observation(id core.AccountID, g core.CredentialGeneration, active bool) core.QuotaObservation {
	p := core.PermissionAllowed
	state := core.WindowExpired
	short := h.clock.now.Add(-time.Minute)
	week := h.clock.now.Add(-time.Hour)
	if active {
		state = core.WindowActive
		short = h.clock.now.Add(5 * time.Hour)
		week = h.clock.now.Add(7 * 24 * time.Hour)
	}
	return core.QuotaObservation{AccountID: id, Generation: g, ReceivedAt: h.clock.now, Permission: &p, FiveHour: &core.WindowEvidence{State: state, ResetAt: short}, Weekly: &core.WindowEvidence{State: state, ResetAt: week}}
}
func (h *fakeHost) ObserveQuota(_ context.Context, q core.QuotaRead) (core.QuotaObservation, error) {
	h.reads++
	o := h.observation(q.AccountID, q.Generation, h.active)
	if h.queryHook != nil {
		h.queryHook()
	}
	return o, nil
}
func (h *fakeHost) SendExact(_ context.Context, q core.ExactRequest) (core.SendResult, error) {
	if !h.store.dispatchSeen {
		panic("send before durable intent")
	}
	h.sends = append(h.sends, q)
	if h.unknown {
		return core.SendResult{Disposition: core.SendUnknown}, errors.New("connection lost")
	}
	o := h.observation(q.AccountID, q.Generation, true)
	return core.SendResult{Disposition: core.SendCompleted, Observation: &o}, nil
}
func fixture(t *testing.T, night, weekly bool) (*Engine, *fakeHost, *memoryStore, *testClock) {
	t.Helper()
	hour := 10
	if night {
		hour = 2
	}
	clock := &testClock{time.Date(2026, 10, 11, hour, 0, 0, 0, time.FixedZone("CST", 8*3600))}
	cfg := config.Defaults()
	cfg.Enabled, cfg.QuotaQuery.Automatic = true, true
	cfg.Execution.Model = "codex-test"
	cfg.ResetRules.FiveHour.RandomDelay = config.DelayRange{}
	cfg.ResetRules.Weekly.RandomDelay = config.DelayRange{}
	cfg.ResetRules.Weekly.Enabled = weekly
	a := core.AccountState{ID: "a", Generation: "g", Enabled: true, FiveHourBaseline: core.WindowBaseline{CycleID: "short-old", StableResetAt: clock.now.Add(-time.Minute)}}
	if weekly {
		a.WeeklyBaseline = core.WindowBaseline{CycleID: "week-old", StableResetAt: clock.now.Add(-time.Hour)}
	}
	store := &memoryStore{s: core.CoreState{Accounts: map[core.AccountID]core.AccountState{"a": a}, Activity: map[core.UserIdentity]core.UserActivity{}, Requests: map[core.RequestID]core.RequestActivity{}}}
	host := &fakeHost{clock: clock, store: store, initial: []core.HostAccount{{ID: "a", Generation: "g", Enabled: true}}}
	e, err := New(cfg, host, store, clock, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	return e, host, store, clock
}
func addUser(e *Engine, c *testClock) {
	e.EnqueueActivity(core.ActivityEvent{RequestID: "r", Identity: "u", Phase: core.ActivityCompletionOnly, At: c.now})
}
func TestWeeklyBypassesNightAndUserLimitAndMerges(t *testing.T) {
	e, h, _, _ := fixture(t, true, true)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 || h.sends[0].AccountID != "a" {
		t.Fatalf("sends %v", h.sends)
	}
	if len(e.Snapshot().Reservations) != 0 {
		t.Fatal("verified activation must release reservation")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("duplicate weekly dispatch")
	}
}
func TestOrdinaryRequiresDemandEvenDuringDay(t *testing.T) {
	for _, night := range []bool{false, true} {
		e, h, _, c := fixture(t, night, false)
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(h.sends) != 0 {
			t.Fatal("empty pool demand sent")
		}
		addUser(e, c)
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(h.sends) != 1 {
			t.Fatalf("active user should permit wake at night=%v", night)
		}
	}
}
func TestForegroundEvidenceCancelsPendingWake(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	h.active = true
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("active window sent")
	}
}
func TestForegroundStartsDuringPreflightBlocksSend(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	h.queryHook = func() {
		e.EnqueueActivity(core.ActivityEvent{RequestID: "inflight", Phase: core.ActivityStarted, Identity: "u", AccountID: "a", Generation: "g", At: c.now})
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("sent despite foreground binding")
	}
}
func TestCommitFailurePreventsHostCall(t *testing.T) {
	e, h, s, c := fixture(t, false, false)
	addUser(e, c)
	s.failDispatch = true
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("wanted durable error")
	}
	if len(h.sends) != 0 {
		t.Fatal("send on uncommitted intent")
	}
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("poisoned engine resumed")
	}
}
func TestForegroundFinishesDuringQueryRejectsEarlierWindowEvidence(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	addUser(e, c)
	h.queryHook = func() {
		e.EnqueueActivity(core.ActivityEvent{RequestID: "during", Phase: core.ActivityStarted, Identity: "u", AccountID: "a", Generation: "g", At: c.now})
		e.EnqueueActivity(core.ActivityEvent{RequestID: "during", Phase: core.ActivityCompleted, At: c.now})
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("inactive query spanning foreground execution was reused")
	}
}
func TestActivityCleanupPreservesUnfinishedRequests(t *testing.T) {
	e, _, _, c := fixture(t, false, false)
	old := c.now.Add(-48 * time.Hour)
	e.state.Requests = map[core.RequestID]core.RequestActivity{"done": {Identity: "old", Completed: true, LastEventAt: old}, "live": {Identity: "live", LastEventAt: old}}
	e.state.Activity = map[core.UserIdentity]core.UserActivity{"old": {Identity: "old", LastActivity: old}, "live": {Identity: "live", LastActivity: old, InFlightCount: 1}}
	e.pruneActivity(c.now)
	if _, ok := e.state.Requests["done"]; ok {
		t.Fatal("old completion retained")
	}
	if _, ok := e.state.Activity["old"]; ok {
		t.Fatal("inactive identity retained")
	}
	if len(e.state.Requests) != 1 || e.state.Activity["live"].InFlightCount != 1 {
		t.Fatal("unfinished request aged out")
	}
}
func TestDisablingFiveHourRulePreservesIntentButStopsDispatch(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := e.cfg
	updated.ResetRules.FiveHour.Enabled = false
	if err := e.UpdateConfig(updated); err != nil {
		t.Fatal(err)
	}
	addUser(e, c)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("disabled five-hour rule sent old intent")
	}
	updated.ResetRules.FiveHour.Enabled = true
	if err := e.UpdateConfig(updated); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("reenabled rule did not catch up")
	}
}
func TestUncertainDispatchSurvivesRestartAndCapacityExpiry(t *testing.T) {
	e, h, s, c := fixture(t, false, false)
	addUser(e, c)
	h.unknown = true
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("initial send")
	}
	c.now = c.now.Add(time.Hour)
	e2, err := New(e.cfg, h, s, c, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(e2, c)
	if err = e2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("ambiguous send retried after restart")
	}
	state := e2.Snapshot()
	if len(state.Reservations) != 0 {
		t.Fatal("capacity should expire")
	}
	if state.Accounts["a"].Task.OperationID == "" {
		t.Fatal("capacity expiry cleared suppression")
	}
}
func TestRatioSubtractsReadyPool(t *testing.T) {
	e, h, _, c := fixture(t, false, false)
	e.cfg.Activity.AccountsPerUser = .5
	addUser(e, c)
	b := core.HostAccount{ID: "b", Generation: "g", Enabled: true}
	h.initial = append(h.initial, b)
	e.state.Accounts["b"] = core.AccountState{ID: "b", Generation: "g", Enabled: true, OrdinaryParticipant: true, FiveHour: core.WindowEvidence{State: core.WindowActive, ResetAt: c.now.Add(time.Hour), ConfirmedAt: c.now}, Permission: core.PermissionAllowed, PermissionObservedAt: c.now}
	e.nextRead["b"] = c.now.Add(time.Minute)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("ready capacity was not subtracted")
	}
}
func TestImmutableRandomDelayAndOutageCatchUp(t *testing.T) {
	e, h, s, c := fixture(t, false, false)
	e.cfg.ResetRules.FiveHour.RandomDelay = config.DelayRange{Min: config.Duration(10 * time.Minute), Max: config.Duration(10 * time.Minute)}
	addUser(e, c)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 0 {
		t.Fatal("sent before jitter")
	}
	due := e.Snapshot().Accounts["a"].Task.Intents[0].RandomizedDueAt
	c.now = c.now.Add(12 * time.Minute)
	e2, err := New(e.cfg, h, s, c, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(e2, c)
	if err = e2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sends) != 1 {
		t.Fatal("did not catch up")
	}
	if !e2.Snapshot().Accounts["a"].Task.Intents[0].RandomizedDueAt.Equal(due) {
		t.Fatal("jitter resampled")
	}
}
