// Package runtime serializes durable transitions and exact-account execution.
package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type WallClock struct{}

func (WallClock) Now() time.Time { return time.Now() }

type CryptoRandom struct{}

func (CryptoRandom) Duration(lo, hi time.Duration) (time.Duration, error) {
	if lo < 0 || hi < lo {
		return 0, errors.New("invalid random range")
	}
	n, err := rand.Int(rand.Reader, new(big.Int).Add(big.NewInt(int64(hi-lo)), big.NewInt(1)))
	if err != nil {
		return 0, err
	}
	return lo + time.Duration(n.Int64()), nil
}

type Status struct {
	State                core.CoreState
	LastError            string
	Capabilities         core.HostCapabilities
	EffectiveConcurrency int
}
type Engine struct {
	published       atomic.Pointer[publishedStatus]
	mu              sync.Mutex
	queueMu         sync.Mutex
	activity        []core.ActivityEvent
	observations    []core.QuotaObservation
	wake            chan struct{}
	cfg             config.Config
	host            core.Host
	store           core.Store
	clock           core.Clock
	random          core.Random
	state           core.CoreState
	caps            core.HostCapabilities
	lastError       string
	poisoned        bool
	nextRead        map[core.AccountID]time.Time
	nextPreflight   map[core.AccountID]time.Time
	readFailures    map[core.AccountID]int
	activityEpoch   map[core.AccountID]uint64
	inventory       []core.HostAccount
	inventoryFailed bool
	nextInventory   time.Time
}

func New(cfg config.Config, host core.Host, store core.Store, clock core.Clock, random core.Random) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if host == nil || store == nil || clock == nil || random == nil {
		return nil, errors.New("runtime dependencies required")
	}
	cfg = copyConfig(cfg)
	s, err := store.Load(context.Background())
	if err != nil {
		return nil, err
	}
	core.RecoverAfterRestart(&s, clock.Now())
	e := &Engine{cfg: cfg, host: host, store: store, clock: clock, random: random, state: s, wake: make(chan struct{}, 1), nextRead: map[core.AccountID]time.Time{}, nextPreflight: map[core.AccountID]time.Time{}, readFailures: map[core.AccountID]int{}, activityEpoch: map[core.AccountID]uint64{}}
	if err = e.commit(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}
func copyConfig(c config.Config) config.Config {
	b, _ := json.Marshal(c)
	var out config.Config
	_ = json.Unmarshal(b, &out)
	return out
}
func cloneState(s core.CoreState) core.CoreState {
	b, _ := json.Marshal(s)
	var out core.CoreState
	_ = json.Unmarshal(b, &out)
	return out
}
func (e *Engine) Snapshot() core.CoreState {
	v := e.published.Load()
	if v == nil {
		return core.CoreState{}
	}
	return cloneState(v.state)
}
func (e *Engine) Status() Status {
	v := e.published.Load()
	if v == nil {
		return Status{EffectiveConcurrency: 1}
	}
	return Status{cloneState(v.state), v.lastError, v.caps, 1}
}
func (e *Engine) Diagnostics() (core.HostCapabilities, string, int) {
	v := e.published.Load()
	if v == nil {
		return core.HostCapabilities{}, "", 1
	}
	return v.caps, v.lastError, 1
}
func (e *Engine) UpdateConfig(c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg = copyConfig(c)
	e.publish(false)
	e.mu.Unlock()
	e.Notify()
	return nil
}
func (e *Engine) Notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) EnqueueActivity(a core.ActivityEvent) {
	e.queueMu.Lock()
	e.activity = append(e.activity, a)
	e.queueMu.Unlock()
	e.Notify()
}
func (e *Engine) EnqueueObservation(o core.QuotaObservation) {
	e.queueMu.Lock()
	e.observations = append(e.observations, o)
	e.queueMu.Unlock()
	e.Notify()
}
func (e *Engine) drain() {
	e.queueMu.Lock()
	a, o := e.activity, e.observations
	e.activity = nil
	e.observations = nil
	e.queueMu.Unlock()
	for _, v := range a {
		old := e.state.Requests[v.RequestID]
		if old.AccountID != "" {
			e.activityEpoch[old.AccountID]++
		}
		if v.AccountID != "" && v.AccountID != old.AccountID {
			e.activityEpoch[v.AccountID]++
		}
		core.ApplyActivity(&e.state, v)
	}
	for _, v := range o {
		if core.ApplyObservation(&e.state, v) {
			a := e.state.Accounts[v.AccountID]
			// Only a complete, accepted foreground snapshot can postpone a
			// background read. Partial/unknown fields, expired boundaries and
			// in-flight delivery verification retain their existing schedule.
			if v.Class == core.ObservationExecution && (v.Source == "cpa_response_headers" || v.Source == "cpa_websocket_quota") &&
				v.FiveHour != nil && v.Weekly != nil && a.Permission == core.PermissionAllowed &&
				a.FiveHour.Remaining != nil && a.Weekly.Remaining != nil && a.FiveHour.Remaining.Value > 0 && a.Weekly.Remaining.Value > 0 &&
				a.FiveHour.State == core.WindowActive && a.Weekly.State == core.WindowActive &&
				a.FiveHour.ConfirmedAt.Equal(v.ReceivedAt) && a.Weekly.ConfirmedAt.Equal(v.ReceivedAt) &&
				a.FiveHour.ResetAt.After(e.clock.Now()) && a.Weekly.ResetAt.After(e.clock.Now()) &&
				(a.Task == nil || a.Task.OperationID == "") {
				e.nextRead[v.AccountID] = maxTime(e.nextRead[v.AccountID], v.ReceivedAt.Add(30*time.Second))
			}
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func (e *Engine) commit(ctx context.Context) error {
	// Once a transition exists, cancellation stops network work but must not
	// discard durable delivery evidence. The store is local and remains bounded.
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	r, err := e.store.Commit(ctx, e.state.Revision, e.state)
	if err != nil {
		e.poisoned = true
		e.lastError = "durable_commit_failed"
		return errors.New("durable_commit_failed")
	}
	e.state.Revision = r
	e.publish(true)
	return nil
}
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := e.Step(ctx)
		if err != nil {
			e.mu.Lock()
			bad := e.poisoned
			e.mu.Unlock()
			if bad {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-e.wake:
		}
	}
}
func (e *Engine) generate(now time.Time) error {
	if !e.cfg.Enabled {
		return nil
	}
	for _, id := range e.ids() {
		p := e.cfg.Account(id)
		if err := core.GenerateResetIntents(&e.state, id, now, core.ResetPolicy{FiveHourEnabled: p.FiveHourEnabled, WeeklyEnabled: p.WeeklyEnabled, FiveHourDelay: core.DelayRange{Min: p.FiveHourRandomDelay.Min.Value(), Max: p.FiveHourRandomDelay.Max.Value()}, WeeklyDelay: core.DelayRange{Min: p.WeeklyRandomDelay.Min.Value(), Max: p.WeeklyRandomDelay.Max.Value()}}, e.random); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) ids() []core.AccountID {
	ids := make([]core.AccountID, 0, len(e.state.Accounts))
	for id := range e.state.Accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
func (e *Engine) observe(ctx context.Context, id core.AccountID) error {
	a := e.state.Accounts[id]
	if b, ok := e.host.(interface{ SetBaseline(core.CoreState) }); ok {
		b.SetBaseline(cloneState(e.state))
	}
	q := core.QuotaRead{AccountID: id, Generation: a.Generation, ReadStartedSeq: core.NextSequence(&e.state)}
	epoch := e.activityEpoch[id]
	readCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.QuotaReadTimeout.Value())
	o, err := e.host.ObserveQuota(readCtx, q)
	if err == nil && readCtx.Err() != nil {
		err = readCtx.Err()
	}
	cancel()
	e.drain()
	e.nextRead[id] = e.clock.Now().Add(30 * time.Second)
	if err != nil {
		e.lastError = "quota_read_failed"
		e.readFailed(id)
		return errors.New("quota_read_failed")
	}
	if o.AccountID != id || o.Generation != a.Generation {
		e.lastError = "quota_binding_mismatch"
		e.readFailed(id)
		return errors.New("quota_binding_mismatch")
	}
	o.Class = core.ObservationQuery
	o.ReadStartedSeq = q.ReadStartedSeq
	if epoch != e.activityEpoch[id] {
		e.lastError = "foreground_changed_during_quota_read"
		e.nextPreflight[id] = e.nextRead[id]
		return errors.New(e.lastError)
	}
	if !core.ApplyObservation(&e.state, o) {
		// A foreground response received during this query owns the newer
		// field fences. A superseded query is not a failing quota adapter.
		current := e.state.Accounts[id]
		if current.Generation == a.Generation && (current.FiveHour.AcceptedSeq > q.ReadStartedSeq || current.Weekly.AcceptedSeq > q.ReadStartedSeq || current.PermissionAcceptedSeq > q.ReadStartedSeq) {
			return nil
		}
		e.lastError = "quota_evidence_rejected"
		e.readFailed(id)
		return errors.New(e.lastError)
	}
	delete(e.readFailures, id)
	delete(e.nextPreflight, id)
	if len(e.readFailures) == 0 && e.lastError == "quota_read_failed" {
		e.lastError = ""
	}
	return nil
}

// Step uses a single execution slot. Every dispatch performs a new quota read,
// drains foreground hooks, recomputes capacity, then commits before calling host.
func (e *Engine) Step(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.publish(false)
	if e.poisoned {
		return errors.New("durable_commit_failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	before, _ := json.Marshal(e.state)
	e.lastError = ""
	if len(e.readFailures) > 0 {
		e.lastError = "quota_read_failed"
	}
	e.drain()
	e.pruneActivity(e.clock.Now())
	caps, err := e.host.Capabilities(ctx)
	if err != nil {
		e.lastError = "capability_read_failed"
		return e.failStep(ctx, before, e.lastError)
	}
	e.caps = caps
	now := e.clock.Now()
	if e.inventoryFailed && now.Before(e.nextInventory) {
		e.lastError = "account_read_failed"
		return e.failStep(ctx, before, e.lastError)
	}
	if e.inventory == nil || !now.Before(e.nextInventory) {
		e.nextInventory = now.Add(30 * time.Second)
		inventoryCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.QuotaReadTimeout.Value())
		hs, readErr := e.host.Accounts(inventoryCtx)
		if readErr == nil && inventoryCtx.Err() != nil {
			readErr = inventoryCtx.Err()
		}
		cancel()
		if readErr != nil {
			e.inventoryFailed = true
			e.nextInventory = e.clock.Now().Add(30 * time.Second)
			e.lastError = "account_read_failed"
			return e.failStep(ctx, before, e.lastError)
		}
		e.inventoryFailed = false
		e.inventory = append([]core.HostAccount{}, hs...)
		e.nextInventory = now.Add(30 * time.Second)
	}
	core.ReconcileAccounts(&e.state, e.inventory, now)
	for id := range e.nextRead {
		if _, ok := e.state.Accounts[id]; !ok {
			delete(e.nextRead, id)
			delete(e.nextPreflight, id)
			delete(e.readFailures, id)
			delete(e.activityEpoch, id)
		}
	}
	for id, a := range e.state.Accounts {
		p := e.cfg.Account(id)
		a.Enabled = a.Enabled && !p.Paused
		a.OrdinaryParticipant = p.OrdinaryParticipant
		a.WeeklyEnabled = p.WeeklyEnabled
		e.state.Accounts[id] = a
	}
	e.drain()
	core.ReleaseExpiredReservations(&e.state, now)
	if err = e.generate(now); err != nil {
		return err
	}
	if err = reconcileDaily(&e.state, e.cfg, now, e.random); err != nil {
		return err
	}
	reads := 0
	readIDs := e.ids()
	sort.SliceStable(readIDs, func(i, j int) bool {
		ai, aj := e.state.Accounts[readIDs[i]], e.state.Accounts[readIDs[j]]
		vi := ai.Task != nil && ai.Task.OperationID != "" && !now.Before(ai.Task.NextVerificationAt)
		vj := aj.Task != nil && aj.Task.OperationID != "" && !now.Before(aj.Task.NextVerificationAt)
		if vi != vj {
			return vi
		}
		if vi && !ai.Task.NextVerificationAt.Equal(aj.Task.NextVerificationAt) {
			return ai.Task.NextVerificationAt.Before(aj.Task.NextVerificationAt)
		}
		return e.nextRead[readIDs[i]].Before(e.nextRead[readIDs[j]])
	})
	for _, id := range readIDs {
		if ctx.Err() != nil {
			return e.commitChanged(ctx, before)
		}
		a := e.state.Accounts[id]
		verifying := a.Task != nil && a.Task.OperationID != "" && !now.Before(a.Task.NextVerificationAt)
		pendingOperation := a.Task != nil && a.Task.OperationID != ""
		if caps.QuotaAdapterReady && (a.Enabled || pendingOperation) && (verifying || (!pendingOperation && !now.Before(e.nextRead[id]))) && reads < 4 && !now.Before(e.nextPreflight[id]) {
			reads++
			_ = e.observe(ctx, id)
			a = e.state.Accounts[id]
			if verifying && a.Task != nil && a.Task.OperationID != "" {
				delays := e.cfg.Runtime().VerifyDelays
				if a.Task.VerificationStartedAt.IsZero() {
					a.Task.VerificationStartedAt = a.Task.DispatchIntentAt
				}
				a.Task.VerificationAttempt++
				n := a.Task.VerificationAttempt
				if n < len(delays) {
					a.Task.NextVerificationAt = a.Task.VerificationStartedAt.Add(delays[n])
				} else {
					a.Task.NextVerificationAt = e.clock.Now().Add(delays[len(delays)-1])
				}
				e.state.Accounts[id] = a
			}
		}
	}
	now = e.clock.Now()
	if err = e.generate(now); err != nil {
		return err
	}
	e.reconcileDailyCapacity(now)
	if !e.cfg.Enabled || !caps.StableAccountIdentity || !caps.ExactAccountExecution || !caps.QuotaAdapterReady {
		return e.commitChanged(ctx, before)
	}
	if !e.state.LastDispatchAt.IsZero() && now.Before(e.state.LastDispatchAt.Add(e.cfg.Runtime().MinDispatchSpacing)) {
		return e.commitChanged(ctx, before)
	}
	e.attachDaily(now)
	ids := e.ids()
	sort.SliceStable(ids, func(i, j int) bool {
		ai, aj := e.state.Accounts[ids[i]], e.state.Accounts[ids[j]]
		wi, wj := dueWeekly(ai, now), dueWeekly(aj, now)
		if wi != wj {
			return wi
		}
		return oldestDue(ai, now).Before(oldestDue(aj, now))
	})
	for _, id := range ids {
		if ctx.Err() != nil {
			return e.commitChanged(ctx, before)
		}
		a := e.state.Accounts[id]
		if !a.Enabled || a.Task == nil || a.Task.OperationID != "" || !e.hasEligibleDue(a, now) {
			continue
		}
		// Cheap policy gate prevents quota polling due to unmet ordinary demand.
		cap := e.capacity(now, id)
		if !dueWeekly(a, now) && cap.Missing == 0 {
			e.hold(id, core.HoldNoCapacity)
			continue
		}
		p := e.cfg.Account(id)
		u := core.ActiveUserCount(e.state, now, e.cfg.Runtime().ActivityLookback)
		if !dueWeekly(a, now) && !e.cfg.WithinAllowedHours(now, id) && !(hasDue(a, core.ReasonFiveHour, now) && p.AllowOutsideHoursWhenUsersActive && u > 0) {
			e.hold(id, core.HoldOutsideHours)
			continue
		}
		if now.Before(a.BackoffUntil) || core.ForegroundBusy(e.state, id, a.Generation) {
			if now.Before(a.BackoffUntil) {
				e.hold(id, core.HoldBackoff)
			} else {
				e.hold(id, core.HoldForegroundBusy)
			}
			continue
		}
		if now.Before(e.nextPreflight[id]) {
			continue
		}
		if reads >= 8 {
			break
		}
		// Failed/ambiguous evidence is retried by the bounded observation poll,
		// rather than by every scheduler tick. Every admitted send still reads anew.
		if now.Before(e.nextRead[id]) && (a.Permission != core.PermissionAllowed || (a.FiveHour.State != core.WindowInactive && a.FiveHour.State != core.WindowExpired)) {
			e.hold(id, core.HoldQuotaUnknown)
			continue
		}
		reads++
		readFence := e.state.NextReadSequence
		if err = e.observe(ctx, id); err != nil {
			e.hold(id, core.HoldQuotaUnknown)
			continue
		}
		now = e.clock.Now()
		e.drain()
		if ctx.Err() != nil {
			return e.commitChanged(ctx, before)
		}
		e.reconcileDailyCapacity(now)
		a = e.state.Accounts[id]
		if a.Task == nil {
			continue
		}
		if e.cfg.Runtime().ForceObserveBeforeDispatch && (a.FiveHour.ReadStartedSeq <= readFence || a.PermissionAcceptedSeq <= readFence) {
			e.hold(id, core.HoldQuotaUnknown)
			e.nextPreflight[id] = e.nextRead[id]
			continue
		}
		task := *a.Task
		task.Intents = append([]core.Intent(nil), a.Task.Intents...)
		for i, v := range task.Intents {
			if (v.Reason == core.ReasonWeekly && !p.WeeklyEnabled) || (v.Reason == core.ReasonFiveHour && !p.FiveHourEnabled) || (v.Reason == core.ReasonDaily && !p.OrdinaryParticipant) {
				task.Intents[i].Status = core.IntentSuperseded
			}
		}
		week := core.WeeklyUnconfirmed
		if a.Weekly.State == core.WindowInactive || a.Weekly.State == core.WindowExpired {
			for _, v := range task.Intents {
				if v.Reason == core.ReasonWeekly && v.Status == core.IntentPending && !now.Before(v.OriginalDueAt) && !a.Weekly.ConfirmedAt.Before(v.OriginalDueAt) {
					week = core.WeeklyConfirmed
				}
			}
		}
		cap = e.capacity(now, id)
		ad := core.DecideAdmission(core.AdmissionInput{Now: now, Enabled: a.Enabled, AccountID: id, Generation: a.Generation, Task: task, FiveHour: a.FiveHour, QuotaPermission: a.Permission, QuotaObservedAt: a.PermissionObservedAt, QuotaMaxAge: e.cfg.Runtime().EvidenceMaxAge, Weekly: week, WeeklyObservedAt: a.Weekly.ConfirmedAt, ForegroundBusy: core.ForegroundBusy(e.state, id, a.Generation), BackoffUntil: a.BackoffUntil, WithinAllowedHours: e.cfg.WithinAllowedHours(now, id), RecentUsers: core.ActiveUserCount(e.state, now, e.cfg.Runtime().ActivityLookback), AllowActiveUsersOutsideHours: p.AllowOutsideHoursWhenUsersActive, OrdinarySlotsAvailable: cap.Missing, ConcurrencySlotAvailable: true})
		if !ad.Allowed {
			e.hold(id, ad.Hold)
			switch ad.Hold {
			case core.HoldQuotaUnknown, core.HoldQuotaExhausted, core.HoldWeeklyUnconfirmed, core.HoldWeeklyConflict, core.HoldActiveFiveHour:
				e.nextPreflight[id] = e.nextRead[id]
			}
			continue
		}
		var raw [16]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return err
		}
		op := core.OperationID(hex.EncodeToString(raw[:]))
		rid := core.ReservationID(op)
		e.state.Reservations = append(e.state.Reservations, core.Reservation{ID: rid, AccountID: id, Generation: a.Generation, Weekly: ad.WeeklyPriority, CapacityHoldUntil: now.Add(e.cfg.Runtime().VerificationCapacityHold)})
		if err = core.MarkDispatch(&e.state, id, op, rid, ad.CoveredEventIDs, now); err != nil {
			return err
		}
		seq := core.NextSequence(&e.state)
		if err = e.commit(ctx); err != nil {
			return err
		}
		sendCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.RequestTimeout.Value())
		result, sendErr := e.host.SendExact(sendCtx, core.ExactRequest{AccountID: id, Generation: a.Generation, OperationID: op, ReadStartedSeq: seq, Model: e.cfg.Execution.Model, Prompt: e.cfg.Execution.Prompt})
		if sendCtx.Err() != nil && result.Disposition != core.SendDefinitelyNotSent {
			result.Disposition = core.SendUnknown
		}
		cancel()
		if result.Disposition == "" {
			result.Disposition = core.SendUnknown
		}
		if sendErr != nil {
			e.lastError = "send_failed"
		}
		finished := e.clock.Now()
		if result.Disposition == core.SendDefinitelyNotSent && result.RetryAfter.IsZero() {
			result.RetryAfter = finished.Add(time.Minute)
		}
		core.ApplySendResult(&e.state, id, op, result, finished)
		for i := range e.state.Reservations {
			if e.state.Reservations[i].OperationID == op {
				e.state.Reservations[i].CapacityHoldUntil = finished.Add(e.cfg.Runtime().VerificationCapacityHold)
			}
		}
		a = e.state.Accounts[id]
		if a.Task != nil && a.Task.OperationID != "" {
			a.Task.VerificationStartedAt = finished
			a.Task.VerificationAttempt = 0
			a.Task.NextVerificationAt = finished.Add(e.cfg.Runtime().VerifyDelays[0])
			e.state.Accounts[id] = a
		}
		e.drain()
		e.reconcileDailyCapacity(finished)
		return e.commit(ctx)
	}
	return e.commitChanged(ctx, before)
}
func (e *Engine) commitChanged(ctx context.Context, before []byte) error {
	after, _ := json.Marshal(e.state)
	if bytes.Equal(before, after) {
		return nil
	}
	return e.commit(ctx)
}
func (e *Engine) failStep(ctx context.Context, before []byte, code string) error {
	e.drain()
	if err := e.commitChanged(ctx, before); err != nil {
		return err
	}
	return errors.New(code)
}
func (e *Engine) hold(id core.AccountID, h core.HoldReason) {
	a := e.state.Accounts[id]
	if a.Task != nil && a.Task.OperationID == "" {
		a.Task.State = core.TaskWaiting
		a.Task.HoldReason = h
		e.state.Accounts[id] = a
	}
}
func hasDue(a core.AccountState, r core.Reason, now time.Time) bool {
	if a.Task != nil {
		for _, v := range a.Task.Intents {
			if v.Reason == r && v.Status == core.IntentPending && !v.RandomizedDueAt.IsZero() && !now.Before(v.RandomizedDueAt) {
				return true
			}
		}
	}
	return false
}
func dueWeekly(a core.AccountState, now time.Time) bool {
	return a.WeeklyEnabled && hasDue(a, core.ReasonWeekly, now)
}
func (e *Engine) hasEligibleDue(a core.AccountState, now time.Time) bool {
	p := e.cfg.Account(a.ID)
	return (p.WeeklyEnabled && hasDue(a, core.ReasonWeekly, now)) || (p.FiveHourEnabled && hasDue(a, core.ReasonFiveHour, now)) || (p.OrdinaryParticipant && hasDue(a, core.ReasonDaily, now))
}
func oldestDue(a core.AccountState, now time.Time) time.Time {
	var t time.Time
	if a.Task != nil {
		for _, v := range a.Task.Intents {
			if v.Status == core.IntentPending && !v.RandomizedDueAt.IsZero() && !now.Before(v.RandomizedDueAt) && (t.IsZero() || v.RandomizedDueAt.Before(t)) {
				t = v.RandomizedDueAt
			}
		}
	}
	return t
}

func (e *Engine) capacity(now time.Time, id core.AccountID) core.Capacity {
	return capacityFor(e.cfg, e.state, now, id)
}
func capacityFor(cfg config.Config, state core.CoreState, now time.Time, id core.AccountID) core.Capacity {
	in := core.CapacityInput{RecentUsers: core.ActiveUserCount(state, now, cfg.Runtime().ActivityLookback), AccountsPerUser: cfg.Activity.AccountsPerUser, MaxAccounts: &cfg.Activity.MaxAccounts}
	for k, a := range state.Accounts {
		if a.Enabled && a.OrdinaryParticipant {
			in.Participants = append(in.Participants, k)
			if a.Permission == core.PermissionAllowed && a.FiveHour.State == core.WindowActive && now.Before(a.FiveHour.ResetAt) && !a.PermissionObservedAt.IsZero() && !a.PermissionObservedAt.After(now) && !a.FiveHour.ConfirmedAt.IsZero() && !a.FiveHour.ConfirmedAt.After(now) {
				in.Active = append(in.Active, k)
			}
		}
	}
	for _, r := range state.Reservations {
		if r.CapacityHoldUntil.IsZero() || now.Before(r.CapacityHoldUntil) {
			in.Reserved = append(in.Reserved, r.AccountID)
		}
	}
	if id == "" || cfg.WithinAllowedHours(now, id) {
		for _, d := range state.DailyIntents {
			if d.Status == core.IntentPending && !now.Before(d.RandomizedDueAt) {
				in.DailyMinimum = max(in.DailyMinimum, d.MinimumReadyAccounts)
			}
		}
	}
	c, _ := core.CalculateCapacity(in)
	return c
}
func (e *Engine) reconcileDailyCapacity(now time.Time) {
	c := e.capacity(now, "")
	for i, d := range e.state.DailyIntents {
		if d.Status == core.IntentPending && !now.Before(d.RandomizedDueAt) && (d.MinimumReadyAccounts == 0 || c.Participants > 0) && c.Active >= min(d.MinimumReadyAccounts, min(c.Participants, e.cfg.Activity.MaxAccounts)) {
			e.state.DailyIntents[i].Status = core.IntentSatisfied
		}
	}
	status := map[core.EventID]core.IntentStatus{}
	for _, d := range e.state.DailyIntents {
		status[d.ID] = d.Status
	}
	for id, a := range e.state.Accounts {
		if a.Task != nil {
			covered := map[core.EventID]bool{}
			if a.Task.OperationID != "" {
				for _, eventID := range a.Task.CoveredEventIDs {
					covered[eventID] = true
				}
			}
			for i, v := range a.Task.Intents {
				if v.Reason == core.ReasonDaily && !covered[v.ID] && status[v.ID] != core.IntentPending {
					if status[v.ID] == "" {
						a.Task.Intents[i].Status = core.IntentSuperseded
					} else {
						a.Task.Intents[i].Status = status[v.ID]
					}
				}
			}
			core.ReconcileVerification(&e.state, id, now)
		}
	}
}
func (e *Engine) attachDaily(now time.Time) {
	for _, id := range e.ids() {
		a := e.state.Accounts[id]
		if !a.Enabled || !a.OrdinaryParticipant || !e.cfg.WithinAllowedHours(now, id) || (a.Task != nil && a.Task.OperationID != "") {
			continue
		}
		for _, d := range e.state.DailyIntents {
			if d.Status != core.IntentPending || now.Before(d.RandomizedDueAt) {
				continue
			}
			if a.Task == nil {
				a.Task = &core.MergedTask{AccountID: id, Generation: a.Generation, State: core.TaskPending}
			}
			found := false
			for _, v := range a.Task.Intents {
				if v.ID == d.ID {
					found = true
				}
			}
			if !found {
				a.Task.Intents = append(a.Task.Intents, d.Intent)
				if a.Task.State == core.TaskSatisfied {
					a.Task.State = core.TaskPending
				}
			}
		}
		e.state.Accounts[id] = a
	}
}

// Completion tombstones cover the host's asynchronous callback replay horizon.
// An unfinished request is never released by a guessed time limit.
func (e *Engine) pruneActivity(now time.Time) {
	for id, r := range e.state.Requests {
		if r.Completed && r.LastEventAt.Before(now.Add(-24*time.Hour)) {
			delete(e.state.Requests, id)
		}
	}
	for id, u := range e.state.Activity {
		if u.InFlightCount == 0 && u.LastActivity.Before(now.Add(-e.cfg.Runtime().ActivityLookback)) {
			delete(e.state.Activity, id)
		}
	}
}
