// Package core describes the scheduler domain. It contains no background worker,
// network client, or durable store implementation.
package core

import "time"

type AccountID string
type CredentialGeneration string
type EventID string
type OperationID string
type ReservationID string
type RequestID string

type WindowKind string

const (
	FiveHour WindowKind = "five_hour"
	Weekly   WindowKind = "weekly"
)

type WindowState string

const (
	WindowUnknown  WindowState = "unknown"
	WindowInactive WindowState = "inactive"
	WindowActive   WindowState = "active"
	WindowExpired  WindowState = "expired"
)

// Evidence describes a classified observation, not a guessed now+duration timer.
// Inactive requires adapter-specific positive evidence; zero usage alone is not
// sufficient. CycleID is a durable local identifier, not a raw reset timestamp.
type WindowEvidence struct {
	Kind           WindowKind
	State          WindowState
	CycleID        string
	ResetAt        time.Time
	Remaining      *QuotaAmount
	ConfirmedAt    time.Time
	Source         string
	OperationID    OperationID
	ReadStartedSeq uint64
	AcceptedSeq    uint64
}

// Units remain as reported by the upstream (percent, tokens, etc.).
type QuotaAmount struct {
	Value float64
	Unit  string
}

type Permission string

const (
	PermissionUnknown Permission = "unknown"
	PermissionAllowed Permission = "allowed"
	PermissionBlocked Permission = "blocked"
)

// Observation fields are optional so a partial response does not erase evidence.
// ReadStartedSeq is allocated before a query/request starts. Query responses
// cannot overwrite fields accepted after that query began. Execution responses
// have their own fresh acceptance fence, so a long foreground request is not
// discarded merely because it started before a newer quota query.
type QuotaObservation struct {
	AccountID      AccountID
	Generation     CredentialGeneration
	ReadStartedSeq uint64
	ReceivedAt     time.Time
	Source         string
	Class          ObservationClass
	Permission     *Permission
	FiveHour       *WindowEvidence
	Weekly         *WindowEvidence
}

type ObservationClass string

const (
	ObservationQuery     ObservationClass = "query"
	ObservationExecution ObservationClass = "execution_response"
)

type WindowBaseline struct {
	CycleID                 string
	StableResetAt           time.Time
	LastConfirmedActivation time.Time
	LastConsumedAt          time.Time
}

type AccountState struct {
	ID                    AccountID
	Generation            CredentialGeneration
	Availability          string
	Enabled               bool
	OrdinaryParticipant   bool
	WeeklyEnabled         bool
	FiveHour              WindowEvidence
	Weekly                WindowEvidence
	FiveHourBaseline      WindowBaseline
	WeeklyBaseline        WindowBaseline
	Permission            Permission
	PermissionObservedAt  time.Time
	PermissionAcceptedSeq uint64
	BackoffUntil          time.Time
	LastRealRequestAt     time.Time
	FiveHourExpiryQueried time.Time
	WeeklyExpiryQueried   time.Time
	Task                  *MergedTask
	EarlyRecoveryAt       time.Time
}

type Reason string

const (
	ReasonFiveHour   Reason = "five_hour_reset"
	ReasonWeekly     Reason = "weekly_reset"
	ReasonDaily      Reason = "daily_schedule"
	ReasonManual     Reason = "manual_wake"
	ReasonGeneration Reason = "prompt_generation"
)

type IntentStatus string

const (
	IntentPending    IntentStatus = "pending"
	IntentSatisfied  IntentStatus = "satisfied"
	IntentSuperseded IntentStatus = "superseded"
)

// RandomizedDueAt is assigned once, persisted with this event, and is immutable.
type Intent struct {
	ID              EventID
	Reason          Reason
	CycleID         string
	RuleID          string
	OriginalDueAt   time.Time
	RandomizedDueAt time.Time
	CreatedAt       time.Time
	Status          IntentStatus
}

type TaskState string

const (
	TaskPending          TaskState = "pending"
	TaskWaiting          TaskState = "waiting"
	TaskReserved         TaskState = "reserved"
	TaskDispatchIntent   TaskState = "dispatch_intent"
	TaskVerifying        TaskState = "verifying"
	TaskAwaitingEvidence TaskState = "awaiting_evidence"
	TaskSatisfied        TaskState = "satisfied"
	TaskSuspended        TaskState = "suspended"
)

type MergedTask struct {
	AccountID                 AccountID
	Generation                CredentialGeneration
	Intents                   []Intent
	State                     TaskState
	NextCheckAt               time.Time
	HoldReason                HoldReason
	ReservationID             ReservationID
	OperationID               OperationID
	DispatchIntentAt          time.Time
	CoveredEventIDs           []EventID
	SendOutcome               SendDisposition
	NextVerificationAt        time.Time
	VerificationStartedAt     time.Time
	VerificationAttempt       int
	SelectedModel             string
	SelectedPrompt            string
	DispatchReadSequence      uint64
	GenerationBaselineResetAt time.Time
}

// PoolDailyIntent exists before any account is selected. Daily rules express
// capacity targets; they do not create one ping task per account.
type PoolDailyIntent struct {
	Intent
	MinimumReadyAccounts int
}

type Reservation struct {
	ID          ReservationID
	AccountID   AccountID
	Generation  CredentialGeneration
	OperationID OperationID
	Weekly      bool
	Sent        bool
	// Releasing this capacity hold never clears the task's delivery suppression.
	CapacityHoldUntil time.Time
}

// UserIdentity is a host-verified ID or locally keyed irreversible key digest.
// Never populate it from an untrusted client user-id field or a raw API key.
type UserIdentity string

type UserActivity struct {
	Identity      UserIdentity
	LastActivity  time.Time
	InFlightCount int
}

type CoreState struct {
	Revision           uint64
	NextReadSequence   uint64
	Accounts           map[AccountID]AccountState
	DailyIntents       []PoolDailyIntent
	Reservations       []Reservation
	Activity           map[UserIdentity]UserActivity
	Requests           map[RequestID]RequestActivity
	LastDispatchAt     time.Time
	History            []HistoryEntry
	GenerationJob      *PromptGenerationJob
	GenerationAccounts map[AccountID]GenerationAccount
}

// Completed bindings are tombstones: repeated finish callbacks cannot count a
// request twice or reopen it. The runtime may prune them after its replay horizon.
type RequestActivity struct {
	Identity    UserIdentity
	AccountID   AccountID
	Generation  CredentialGeneration
	StartedAt   time.Time
	LastEventAt time.Time
	Completed   bool
}

type DelayRange struct{ Min, Max time.Duration }

type ResetPolicy struct {
	FiveHourEnabled, WeeklyEnabled bool
	FiveHourDelay, WeeklyDelay     DelayRange
}
