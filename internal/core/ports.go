package core

import (
	"context"
	"time"
)

type Clock interface{ Now() time.Time }

// Duration returns a sample in the inclusive [min,max] range or an error for
// invalid bounds. The orchestrator calls it only when creating a durable event.
type Random interface {
	Duration(min, max time.Duration) (time.Duration, error)
}

// Store commits an entire transition with compare-and-swap. expectedRevision
// must match; success increments CoreState.Revision. It must durably commit the
// dispatch intent before the caller invokes SendExact. FileStore implements this.
type Store interface {
	Load(context.Context) (CoreState, error)
	Commit(ctx context.Context, expectedRevision uint64, next CoreState) (uint64, error)
}

type HostCapabilities struct {
	StableAccountIdentity bool
	ExactAccountExecution bool
	// QuotaAdapterReady describes our adapter, not an assumed host.quota RPC.
	QuotaAdapterReady            bool
	ActivityIdentityAtStart      bool
	ActivityIdentityAtCompletion bool
	ForegroundGuard              bool
}

// Availability preserves the host status separately from plugin pause policy.
const (
	AccountAvailable          = "available"
	AccountDisabled           = "disabled"
	AccountUnavailable        = "unavailable"
	AccountBindingUnavailable = "binding_unavailable"
)

type HostAccount struct {
	ID           AccountID
	Generation   CredentialGeneration
	Enabled      bool
	Availability string
}

type QuotaRead struct {
	AccountID      AccountID
	Generation     CredentialGeneration
	ReadStartedSeq uint64
}

type ExactRequest struct {
	AccountID      AccountID
	Generation     CredentialGeneration
	OperationID    OperationID
	ReadStartedSeq uint64
	Model          string
	Prompt         string
}

type SendDisposition string

const (
	// This is the only retryable disposition without further delivery evidence.
	SendDefinitelyNotSent SendDisposition = "definitely_not_sent"
	SendCompleted         SendDisposition = "completed"
	SendRejected          SendDisposition = "rejected"
	SendUnknown           SendDisposition = "unknown"
)

type SendResult struct {
	Disposition SendDisposition
	// Observation is the latest message, retained for single-observation hosts.
	Observation *QuotaObservation
	// Observations preserves distinct original messages in receive order. The
	// adapter retains at most five: latest permission, window fields and positive
	// activation evidence. Fields and timestamps must never be merged across messages.
	// When nonempty, this list takes precedence over Observation.
	Observations []QuotaObservation
	RetryAfter   time.Time
	ErrorCode    string // sanitized classification, no upstream body or credential
}

// Host is a proposed adapter boundary, not a CPA plugin SDK contract.
// SendExact must revalidate the account binding and fail without sending if a
// credential change is detected. With ForegroundGuard=true the adapter must
// atomically deny a send already satisfied by foreground execution. With that
// capability absent, observation checks are best effort, not global exclusion.
// It must consume/classify a complete stream; HTTP 200 is not SendCompleted.
// OperationID provides local correlation, not assumed upstream idempotency.
type Host interface {
	Capabilities(context.Context) (HostCapabilities, error)
	Accounts(context.Context) ([]HostAccount, error)
	ObserveQuota(context.Context, QuotaRead) (QuotaObservation, error)
	SendExact(context.Context, ExactRequest) (SendResult, error)
}

type ActivityPhase string

const (
	ActivityStarted         ActivityPhase = "started"
	ActivityAccountSelected ActivityPhase = "account_selected"
	ActivityCompleted       ActivityPhase = "completed"
	ActivityCompletionOnly  ActivityPhase = "completion_only"
)

// ActivityEvent must originate from host authentication and lifecycle hooks.
// Adapter traffic (including every SendExact and quota query) is excluded.
// RequestID is the host's stable execution ID. Repeated account_selected events
// update the same request binding; completed is idempotent and resolves the
// original identity through that binding, even if completion metadata is partial.
// Start/finish accounting is optional: a host that only exposes an authenticated
// identity on completion emits completion activity and cannot maintain trusted
// in-flight-user counts. Adapters must advertise this limitation explicitly.
type ActivityEvent struct {
	RequestID  RequestID
	Phase      ActivityPhase
	Identity   UserIdentity
	AccountID  AccountID
	Generation CredentialGeneration
	At         time.Time
}
