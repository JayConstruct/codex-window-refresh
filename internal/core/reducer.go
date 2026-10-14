package core

import (
	"fmt"
	"time"
)

// ReconcileAccounts treats the host's inventory as authoritative. Configured
// participation overrides are applied by the runtime after this transition.
func ReconcileAccounts(s *CoreState, hosts []HostAccount, now time.Time) {
	if s.Accounts == nil {
		s.Accounts = make(map[AccountID]AccountState)
	}
	seen := make(map[AccountID]bool)
	for _, h := range hosts {
		if h.ID == "" || h.Generation == "" {
			continue
		}
		seen[h.ID] = true
		a, found := s.Accounts[h.ID]
		if !found || a.Generation != h.Generation {
			releaseAccount(s, h.ID)
			a = AccountState{ID: h.ID, Generation: h.Generation, OrdinaryParticipant: true, WeeklyEnabled: true, Permission: PermissionUnknown}
		}
		a.Enabled = h.Enabled
		if !a.Enabled && a.Task != nil {
			a.Task.State = TaskSuspended
			releaseAccount(s, h.ID)
		}
		if a.Enabled && a.Task != nil && a.Task.State == TaskSuspended {
			if a.Task.OperationID != "" {
				a.Task.State = TaskAwaitingEvidence
			} else {
				a.Task.State = TaskPending
			}
		}
		s.Accounts[h.ID] = a
	}
	for id := range s.Accounts {
		if !seen[id] {
			releaseAccount(s, id)
			delete(s.Accounts, id)
			continue
		}
	}
	_ = now
}

// NextSequence allocates query-start and field-acceptance fences in the same
// serialized state transaction. Sequence zero is reserved for absent evidence.
func NextSequence(s *CoreState) uint64 { s.NextReadSequence++; return s.NextReadSequence }

// ApplyObservation accepts each present field independently. Old-generation
// responses and stale queries have no effect, including on task verification.
func ApplyObservation(s *CoreState, o QuotaObservation) bool {
	a, ok := s.Accounts[o.AccountID]
	if !ok || a.Generation != o.Generation || o.ReceivedAt.IsZero() {
		return false
	}
	if o.Class != ObservationQuery && o.Class != ObservationExecution {
		return false
	}
	if o.Class == ObservationQuery && o.ReadStartedSeq == 0 {
		return false
	}
	accept := func(seq uint64, at time.Time) bool {
		return (o.Class == ObservationExecution || o.ReadStartedSeq >= seq) && !o.ReceivedAt.Before(at)
	}
	earlyRecovery := o.Permission != nil && *o.Permission == PermissionAllowed && accept(a.PermissionAcceptedSeq, a.PermissionObservedAt) &&
		bothFull(o.FiveHour, o.Weekly) && consumedFutureWindow(a.FiveHour, a.FiveHourBaseline, o.ReceivedAt) && consumedFutureWindow(a.Weekly, a.WeeklyBaseline, o.ReceivedAt) &&
		accept(a.FiveHour.AcceptedSeq, a.FiveHour.ConfirmedAt) && accept(a.Weekly.AcceptedSeq, a.Weekly.ConfirmedAt)
	if earlyRecovery {
		oldShort, oldWeek := a.FiveHourBaseline, a.WeeklyBaseline
		short, week := *o.FiveHour, *o.Weekly
		short.State, short.CycleID, week.State, week.CycleID = WindowInactive, "", WindowInactive, ""
		o.FiveHour, o.Weekly = &short, &week
		a.FiveHourBaseline, a.WeeklyBaseline = WindowBaseline{}, WindowBaseline{}
		a.EarlyRecoveryAt = o.ReceivedAt
		if a.Task != nil {
			covered := map[EventID]bool{}
			if a.Task.OperationID != "" {
				for _, event := range a.Task.CoveredEventIDs {
					covered[event] = true
				}
			}
			for index := range a.Task.Intents {
				intent := &a.Task.Intents[index]
				if intent.Status == IntentPending && !covered[intent.ID] && ((intent.Reason == ReasonFiveHour && intent.CycleID == oldShort.CycleID) || (intent.Reason == ReasonWeekly && intent.CycleID == oldWeek.CycleID)) {
					intent.Status = IntentSuperseded
				}
			}
		}
		AppendHistory(s, HistoryEntry{Kind: "reset", AccountID: a.ID, Generation: a.Generation, StartedAt: o.ReceivedAt, FinishedAt: o.ReceivedAt, Status: "completed", Sources: []ActionSource{
			{Trigger: "early_recovery", EvidenceSource: o.Source, EvidenceAt: o.ReceivedAt, ResetAt: oldShort.StableResetAt, CycleID: oldShort.CycleID},
			{Trigger: "early_recovery", EvidenceSource: o.Source, EvidenceAt: o.ReceivedAt, ResetAt: oldWeek.StableResetAt, CycleID: oldWeek.CycleID},
		}})
	}
	resetClassified := func(w *WindowEvidence) bool {
		return w != nil && (w.State == WindowInactive || (w.State == WindowExpired && !w.ResetAt.IsZero() && !o.ReceivedAt.Before(w.ResetAt)))
	}
	// A weekly joint reset is the supported exception to a still-running short
	// countdown. Require both fields in this response to pass their own fences;
	// a stale previous-week expiration cannot supply that exception.
	jointResetProof := resetClassified(o.FiveHour) && resetClassified(o.Weekly) &&
		accept(a.FiveHour.AcceptedSeq, a.FiveHour.ConfirmedAt) && accept(a.Weekly.AcceptedSeq, a.Weekly.ConfirmedAt) &&
		!a.WeeklyBaseline.StableResetAt.IsZero() && !o.ReceivedAt.Before(a.WeeklyBaseline.StableResetAt) &&
		(o.Weekly.State != WindowExpired || !o.Weekly.ResetAt.Before(a.WeeklyBaseline.StableResetAt))
	changed := false
	if o.Permission != nil && accept(a.PermissionAcceptedSeq, a.PermissionObservedAt) {
		a.Permission = *o.Permission
		a.PermissionObservedAt = o.ReceivedAt
		a.PermissionAcceptedSeq = NextSequence(s)
		changed = true
	}
	setWindow := func(dst *WindowEvidence, src *WindowEvidence, kind WindowKind) bool {
		if src == nil || !accept(dst.AcceptedSeq, dst.ConfirmedAt) {
			return false
		}
		// A fresh delivery timestamp does not make an older reset boundary a new
		// cycle. Preserve the confirmed countdown even after an intervening
		// unknown observation; otherwise admission could send before its end.
		if kind == FiveHour && src.State == WindowExpired &&
			!a.FiveHourBaseline.StableResetAt.IsZero() && o.ReceivedAt.Before(a.FiveHourBaseline.StableResetAt) &&
			src.ResetAt.Before(a.FiveHourBaseline.StableResetAt) && !jointResetProof {
			return false
		}
		v := *src
		if v.Remaining != nil {
			amount := *v.Remaining
			v.Remaining = &amount
		}
		v.Kind = kind
		v.ConfirmedAt = o.ReceivedAt
		v.Source = o.Source
		v.ReadStartedSeq = o.ReadStartedSeq
		v.AcceptedSeq = NextSequence(s)
		*dst = v
		changed = true
		return true
	}
	shortAccepted := setWindow(&a.FiveHour, o.FiveHour, FiveHour)
	weekAccepted := setWindow(&a.Weekly, o.Weekly, Weekly)
	if !changed {
		return false
	}
	// Never clear a new active short window on the strength of a weekly field.
	// Joint reset requires a positively classified short field in this response.
	jointReset := shortAccepted && weekAccepted && jointResetProof
	if jointReset {
		if a.Task != nil {
			// Only a mature enabled weekly intent can replace a short intent.
			// Disabled or still-jittering weekly policy must not erase ordinary demand.
			weeklyReplacement := false
			for _, e := range a.Task.Intents {
				if a.WeeklyEnabled && e.Reason == ReasonWeekly && e.Status == IntentPending && !e.RandomizedDueAt.IsZero() && !o.ReceivedAt.Before(e.RandomizedDueAt) {
					weeklyReplacement = true
				}
			}
			for i := range a.Task.Intents {
				e := &a.Task.Intents[i]
				if weeklyReplacement && e.Reason == ReasonFiveHour && e.Status == IntentPending && e.CycleID == a.FiveHourBaseline.CycleID {
					e.Status = IntentSuperseded
				}
			}
		}
		a.FiveHourBaseline = WindowBaseline{}
	}
	s.Accounts[o.AccountID] = a
	ReconcileVerification(s, o.AccountID, o.ReceivedAt)
	a = s.Accounts[o.AccountID]
	if !jointReset {
		updateBaseline(&a.FiveHourBaseline, a.FiveHour, o.ReceivedAt)
	}
	updateBaseline(&a.WeeklyBaseline, a.Weekly, o.ReceivedAt)
	s.Accounts[o.AccountID] = a
	return true
}

func updateBaseline(b *WindowBaseline, w WindowEvidence, now time.Time) {
	if w.ResetAt.IsZero() || (w.State != WindowActive && w.State != WindowExpired) {
		return
	}
	// A placeholder cannot move an established countdown backwards.
	if !b.StableResetAt.IsZero() && w.ResetAt.Before(b.StableResetAt) && !(w.State == WindowActive && w.CycleID != "" && w.CycleID != b.CycleID) {
		return
	}
	if b.StableResetAt.Equal(w.ResetAt) && b.CycleID != "" {
		if w.State == WindowActive && w.Remaining != nil && w.Remaining.Unit == "percent" && w.Remaining.Value < 100 && w.Remaining.Value >= 0 {
			b.LastConsumedAt = now
		}
		return
	}
	id := w.CycleID
	if id == "" {
		id = w.ResetAt.UTC().Format(time.RFC3339Nano)
	}
	*b = WindowBaseline{CycleID: id, StableResetAt: w.ResetAt}
	if w.State == WindowActive && w.Remaining != nil && w.Remaining.Unit == "percent" && w.Remaining.Value < 100 && w.Remaining.Value >= 0 {
		b.LastConsumedAt = now
	}
	// This is classification freshness, not an inferred upstream activation time.
	_ = now
}

// GenerateResetIntents persists one immutable random deadline per stable cycle.
// Catch-up uses the original reset boundary, even after a long outage.
func GenerateResetIntents(s *CoreState, id AccountID, now time.Time, policy ResetPolicy, rng Random) error {
	if policy.FiveHourDelay.Min < 0 || policy.FiveHourDelay.Max < policy.FiveHourDelay.Min || policy.WeeklyDelay.Min < 0 || policy.WeeklyDelay.Max < policy.WeeklyDelay.Min || rng == nil {
		return fmt.Errorf("invalid reset delay")
	}
	a, ok := s.Accounts[id]
	if !ok || !a.Enabled {
		return nil
	}
	type candidate struct {
		reason   Reason
		baseline WindowBaseline
		enabled  bool
		delay    DelayRange
	}
	for _, c := range []candidate{{ReasonWeekly, a.WeeklyBaseline, a.WeeklyEnabled && policy.WeeklyEnabled, policy.WeeklyDelay}, {ReasonFiveHour, a.FiveHourBaseline, a.OrdinaryParticipant && policy.FiveHourEnabled, policy.FiveHourDelay}} {
		if !c.enabled || c.baseline.StableResetAt.IsZero() || now.Before(c.baseline.StableResetAt) {
			continue
		}
		eid := EventID(fmt.Sprintf("%s/%s/%s/%s", id, a.Generation, c.reason, c.baseline.CycleID))
		found := false
		if a.Task != nil {
			for _, e := range a.Task.Intents {
				if e.ID == eid {
					found = true
					break
				}
			}
		}
		if found {
			continue
		}
		duration, err := rng.Duration(c.delay.Min, c.delay.Max)
		if err != nil {
			return err
		}
		if duration < c.delay.Min || duration > c.delay.Max {
			return fmt.Errorf("random delay outside bounds")
		}
		if a.Task == nil {
			a.Task = &MergedTask{AccountID: id, Generation: a.Generation, State: TaskPending}
		}
		a.Task.Intents = append(a.Task.Intents, Intent{ID: eid, Reason: c.reason, CycleID: c.baseline.CycleID, OriginalDueAt: c.baseline.StableResetAt, RandomizedDueAt: c.baseline.StableResetAt.Add(duration), CreatedAt: now, Status: IntentPending})
		if a.Task.State == TaskSatisfied {
			a.Task.State = TaskPending
		}
		s.Accounts[id] = a // Preserve any successfully allocated event if the next RNG fails.
	}
	return nil
}

// ApplyActivity uses request bindings rather than incrementing every callback.
// Adapter-generated events must already be filtered by the host adapter.
func ApplyActivity(s *CoreState, e ActivityEvent) {
	if e.RequestID == "" || e.At.IsZero() {
		return
	}
	if s.Requests == nil {
		s.Requests = make(map[RequestID]RequestActivity)
	}
	if s.Activity == nil {
		s.Activity = make(map[UserIdentity]UserActivity)
	}
	r, exists := s.Requests[e.RequestID]
	if exists && (r.Completed || e.At.Before(r.LastEventAt)) {
		return
	}
	switch e.Phase {
	case ActivityStarted:
		if !exists {
			if e.Identity == "" {
				return
			}
			r = RequestActivity{Identity: e.Identity, StartedAt: e.At}
		}
		if e.AccountID != "" {
			r.AccountID = e.AccountID
			r.Generation = e.Generation
		}
	case ActivityAccountSelected:
		if !exists {
			if e.Identity == "" {
				return
			}
			r = RequestActivity{Identity: e.Identity, StartedAt: e.At}
		}
		r.AccountID = e.AccountID
		r.Generation = e.Generation
	case ActivityCompleted:
		if !exists {
			return
		} // Unknown finish cannot manufacture an active user.
		r.Completed = true
	case ActivityCompletionOnly:
		if exists || e.Identity == "" {
			return
		}
		r = RequestActivity{Identity: e.Identity, StartedAt: e.At, Completed: true, AccountID: e.AccountID, Generation: e.Generation}
	default:
		return
	}
	r.LastEventAt = e.At
	s.Requests[e.RequestID] = r
	if r.Identity != "" {
		u := s.Activity[r.Identity]
		u.Identity = r.Identity
		// Request identity is immutable after the first accepted event. Only a
		// new unfinished request and its first completion change occupancy;
		// duplicate starts, account retries, and completion tombstones do not.
		// Rebuilding counts here would scan the whole callback replay history
		// for every event, even though completed requests cannot change counts.
		if !exists && !r.Completed {
			u.InFlightCount++
		} else if exists && r.Completed {
			u.InFlightCount = max(0, u.InFlightCount-1)
		}
		if e.At.After(u.LastActivity) {
			u.LastActivity = e.At
		}
		s.Activity[r.Identity] = u
	}
	if r.AccountID != "" {
		a, ok := s.Accounts[r.AccountID]
		if ok && a.Generation == r.Generation && e.At.After(a.LastRealRequestAt) {
			a.LastRealRequestAt = e.At
			s.Accounts[r.AccountID] = a
		}
	}
}
func recountActivity(s *CoreState) {
	for id, u := range s.Activity {
		u.InFlightCount = 0
		s.Activity[id] = u
	}
	for _, r := range s.Requests {
		if !r.Completed && r.Identity != "" {
			u := s.Activity[r.Identity]
			u.Identity = r.Identity
			u.InFlightCount++
			s.Activity[r.Identity] = u
		}
	}
}
func ActiveUserCount(s CoreState, now time.Time, lookback time.Duration) int {
	if lookback < 0 {
		return 0
	}
	n := 0
	for _, u := range s.Activity {
		if u.Identity != "" && (u.InFlightCount > 0 || (!u.LastActivity.After(now) && !u.LastActivity.Before(now.Add(-lookback)))) {
			n++
		}
	}
	return n
}
func ForegroundBusy(s CoreState, id AccountID, generation CredentialGeneration) bool {
	for _, r := range s.Requests {
		if !r.Completed && r.AccountID == id && r.Generation == generation {
			return true
		}
	}
	return false
}

func MarkDispatch(s *CoreState, id AccountID, op OperationID, reservation ReservationID, covered []EventID, now time.Time) error {
	a, ok := s.Accounts[id]
	if !ok || !a.Enabled || a.Task == nil || op == "" || now.IsZero() {
		return fmt.Errorf("invalid dispatch binding")
	}
	t := a.Task
	if t.Generation != a.Generation || (t.State != TaskPending && t.State != TaskWaiting && t.State != TaskReserved) {
		return fmt.Errorf("dispatch state is suppressed")
	}
	ids := make(map[EventID]bool)
	for _, id := range covered {
		ids[id] = true
	}
	if len(ids) == 0 {
		return fmt.Errorf("no covered events")
	}
	for _, e := range t.Intents {
		if ids[e.ID] && e.Status == IntentPending && !e.RandomizedDueAt.IsZero() && !now.Before(e.RandomizedDueAt) {
			delete(ids, e.ID)
		}
	}
	if len(ids) > 0 {
		return fmt.Errorf("covered events must be mature and pending")
	}
	t.State = TaskDispatchIntent
	t.OperationID = op
	t.ReservationID = reservation
	t.DispatchIntentAt = now
	t.CoveredEventIDs = append([]EventID(nil), covered...)
	t.SendOutcome = ""
	t.HoldReason = HoldAlreadyDispatched
	s.LastDispatchAt = now
	for i := range s.Reservations {
		r := &s.Reservations[i]
		if r.ID == reservation && r.AccountID == id && r.Generation == a.Generation {
			r.OperationID = op
			r.Sent = true
		}
	}
	s.Accounts[id] = a
	return nil
}

// ApplySendResult never turns ambiguous delivery into a retry. Capacity hold
// deadlines are assigned by the runtime; releasing them leaves suppression intact.
func ApplySendResult(s *CoreState, id AccountID, op OperationID, result SendResult, now time.Time) bool {
	a, ok := s.Accounts[id]
	if !ok || a.Task == nil || a.Task.OperationID != op || op == "" {
		return false
	}
	t := a.Task
	if t.State != TaskDispatchIntent && t.State != TaskVerifying && t.State != TaskAwaitingEvidence {
		return false
	}
	if t.SendOutcome != "" {
		return false
	}
	if result.RetryAfter.After(a.BackoffUntil) {
		a.BackoffUntil = result.RetryAfter
	}
	t.SendOutcome = result.Disposition
	switch result.Disposition {
	case SendDefinitelyNotSent:
		t.State = TaskWaiting
		t.OperationID = ""
		t.ReservationID = ""
		t.CoveredEventIDs = nil
		t.DispatchIntentAt = time.Time{}
		t.HoldReason = HoldNone
		releaseAccount(s, id)
	case SendCompleted:
		t.State = TaskVerifying
		t.NextVerificationAt = now
	default:
		t.State = TaskAwaitingEvidence
		t.NextVerificationAt = now
	}
	s.Accounts[id] = a
	observations := result.Observations
	if len(observations) == 0 && result.Observation != nil {
		observations = []QuotaObservation{*result.Observation}
	}
	for _, o := range observations {
		if o.AccountID == id && o.Generation == a.Generation {
			o.Class = ObservationExecution
			ApplyObservation(s, o)
		}
	}
	ReconcileVerification(s, id, now)
	return true
}

func newWindowForIntent(w WindowEvidence, e Intent, now time.Time) bool {
	return w.State == WindowActive && !w.ResetAt.IsZero() && now.Before(w.ResetAt) && !w.ConfirmedAt.Before(e.OriginalDueAt) &&
		((w.CycleID != "" && e.CycleID != "" && w.CycleID != e.CycleID) || w.ResetAt.After(e.OriginalDueAt))
}

// ReconcileVerification satisfies only positively evidenced cycles. A mature
// weekly event requires both the new week and an active short window.
func ReconcileVerification(s *CoreState, id AccountID, now time.Time) bool {
	a, ok := s.Accounts[id]
	if !ok || a.Task == nil {
		return false
	}
	t := a.Task
	changed := false
	covered := make(map[EventID]bool)
	for _, id := range t.CoveredEventIDs {
		covered[id] = true
	}
	sending := t.OperationID != ""
	for i := range t.Intents {
		e := &t.Intents[i]
		if e.Status != IntentPending {
			continue
		}
		if sending && !covered[e.ID] {
			continue
		}
		satisfied := false
		switch e.Reason {
		case ReasonFiveHour:
			satisfied = newWindowForIntent(a.FiveHour, *e, now)
		case ReasonWeekly:
			satisfied = newWindowForIntent(a.Weekly, *e, now) && a.FiveHour.State == WindowActive && now.Before(a.FiveHour.ResetAt) && !a.FiveHour.ConfirmedAt.Before(e.OriginalDueAt)
		case ReasonDaily, ReasonManual:
			satisfied = a.FiveHour.State == WindowActive && now.Before(a.FiveHour.ResetAt) && !a.FiveHour.ConfirmedAt.Before(e.RandomizedDueAt)
		case ReasonGeneration:
			satisfied = a.FiveHour.State == WindowActive && now.Before(a.FiveHour.ResetAt) && !a.FiveHour.ConfirmedAt.Before(e.RandomizedDueAt) && a.FiveHour.AcceptedSeq > t.DispatchReadSequence &&
				(a.FiveHour.OperationID == t.OperationID || !a.FiveHour.ResetAt.Equal(t.GenerationBaselineResetAt))
		}
		if satisfied {
			if !sending && now.Before(e.RandomizedDueAt) {
				e.Status = IntentSuperseded
			} else {
				e.Status = IntentSatisfied
			}
			changed = true
		}
	}
	unresolvedCovered := false
	pending := false
	for _, e := range t.Intents {
		if e.Status == IntentPending {
			pending = true
			if covered[e.ID] {
				unresolvedCovered = true
			}
		}
	}
	if sending && !unresolvedCovered {
		releaseAccount(s, id)
		t.OperationID = ""
		t.ReservationID = ""
		t.CoveredEventIDs = nil
		t.DispatchIntentAt = time.Time{}
		t.NextVerificationAt = time.Time{}
		t.HoldReason = HoldNone
		changed = true
		if pending {
			t.State = TaskPending
		} else {
			t.State = TaskSatisfied
		}
	} else if !sending && !pending && t.State != TaskSatisfied {
		releaseAccount(s, id)
		t.State = TaskSatisfied
		t.HoldReason = HoldNone
		changed = true
	}
	s.Accounts[id] = a
	return changed
}
func releaseAccount(s *CoreState, id AccountID) {
	out := s.Reservations[:0]
	for _, r := range s.Reservations {
		if r.AccountID != id {
			out = append(out, r)
		}
	}
	s.Reservations = out
}
func ReleaseExpiredReservations(s *CoreState, now time.Time) {
	out := s.Reservations[:0]
	for _, r := range s.Reservations {
		if r.CapacityHoldUntil.IsZero() || now.Before(r.CapacityHoldUntil) {
			out = append(out, r)
		}
	}
	s.Reservations = out
}

// ExpireActivityRequests bounds missing-finish occupancy. The host's configured
// request deadline supplies maxInFlight; retention supplies its callback replay
// horizon. Tombstones remain until then so delayed duplicate callbacks are safe.
func ExpireActivityRequests(s *CoreState, now time.Time, maxInFlight, retention time.Duration) {
	if maxInFlight <= 0 || retention <= 0 {
		return
	}
	for id, r := range s.Requests {
		if !r.Completed && !now.Before(r.LastEventAt.Add(maxInFlight)) {
			r.Completed = true
			r.LastEventAt = now
			s.Requests[id] = r
		}
		if r.Completed && !now.Before(r.LastEventAt.Add(retention)) {
			delete(s.Requests, id)
		}
	}
	recountActivity(s)
}

// RecoverAfterRestart clears process-local foreground occupancy and preserves
// durable send uncertainty. It must be persisted before scheduling new work.
func RecoverAfterRestart(s *CoreState, now time.Time) {
	for id, r := range s.Requests {
		if !r.Completed {
			r.Completed = true
			s.Requests[id] = r
		}
	}
	recountActivity(s)
	for id, a := range s.Accounts {
		if a.Task != nil {
			switch a.Task.State {
			case TaskDispatchIntent:
				a.Task.State = TaskAwaitingEvidence
				a.Task.NextVerificationAt = now
				a.Task.HoldReason = HoldAlreadyDispatched
			case TaskVerifying:
				a.Task.State = TaskAwaitingEvidence
				if a.Task.NextVerificationAt.IsZero() {
					a.Task.NextVerificationAt = now
				}
				a.Task.HoldReason = HoldAlreadyDispatched
			case TaskReserved:
				a.Task.State = TaskPending
				a.Task.ReservationID = ""
				releaseAccount(s, id)
			}
		}
		s.Accounts[id] = a
	}
	ReleaseExpiredReservations(s, now)
}
