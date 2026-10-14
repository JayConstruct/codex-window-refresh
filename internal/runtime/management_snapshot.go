package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

// ManagementSnapshot binds all live panel data to one published runtime view.
// SnapshotSeq also advances for progress and configuration changes that do not
// change the durable CoreState revision. InstanceID changes after a restart.
type ManagementSnapshot struct {
	InstanceID           string
	SnapshotSeq          uint64
	State                core.CoreState
	Capabilities         core.HostCapabilities
	Scheduler            SchedulingSummary
	Config               config.Config
	LastError            string
	HostHTTPStatus       string
	InventoryDiagnostics map[core.AccountID]string
}

var snapshotInstanceFallback atomic.Uint64

func newSnapshotInstanceID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	// This identifies process instances, rather than authorizing access.
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), snapshotInstanceFallback.Add(1))
}

func cloneSchedulingSummary(summary SchedulingSummary) SchedulingSummary {
	raw, _ := json.Marshal(summary)
	var copied SchedulingSummary
	_ = json.Unmarshal(raw, &copied)
	return copied
}

func (e *Engine) ManagementSnapshot() ManagementSnapshot {
	v := e.published.Load()
	if v == nil {
		return ManagementSnapshot{}
	}
	return ManagementSnapshot{
		InstanceID: v.instanceID, SnapshotSeq: v.snapshotSeq,
		State: cloneState(v.state), Capabilities: v.caps,
		Scheduler: cloneSchedulingSummary(v.scheduler),
		Config:    copyConfig(v.config), LastError: v.lastError,
		HostHTTPStatus: v.hostHTTPStatus, InventoryDiagnostics: copyInventoryDiagnostics(v.inventoryDiagnostics),
	}
}

func copyInventoryDiagnostics(source map[core.AccountID]string) map[core.AccountID]string {
	if source == nil {
		return nil
	}
	result := make(map[core.AccountID]string, len(source))
	for id, code := range source {
		result[id] = code
	}
	return result
}

// RefreshQuotaConnectionStatus publishes the explicit local operation check
// without waiting for the worker or advancing uncommitted account evidence.
func (e *Engine) RefreshQuotaConnectionStatus() {
	checker, ok := e.host.(interface{ QuotaConnectionStatus() string })
	if !ok {
		return
	}
	e.publicationMu.Lock()
	defer e.publicationMu.Unlock()
	previous := e.published.Load()
	if previous == nil {
		return
	}
	next := *previous
	next.snapshotSeq++
	next.hostHTTPStatus = checker.QuotaConnectionStatus()
	e.published.Store(&next)
}

func applyLatestJobViews(summary *SchedulingSummary) {
	for _, job := range summary.Jobs {
		switch {
		case job.QuotaQuery != nil:
			summary.QuotaQuery = *job.QuotaQuery
		case job.ManualWake != nil:
			summary.ManualWake = *job.ManualWake
		case job.PromptGeneration != nil:
			summary.GenerationJob = job.PromptGeneration
		}
	}
}

// Queue acceptance must be observable even when the worker is still waiting
// for a network call. Only the task registry advances here; account evidence,
// applied configuration and scheduling decisions retain their published view.
// The caller must release queueMu before entering this publication lock.
func (e *Engine) publishQueuedJobs() {
	e.publicationMu.Lock()
	defer e.publicationMu.Unlock()
	previous := e.published.Load()
	if previous == nil {
		return
	}
	next := *previous
	next.snapshotSeq++
	next.scheduler = cloneSchedulingSummary(previous.scheduler)
	next.scheduler.Jobs = e.collectJobStatuses(previous.state)
	next.scheduler.ObservedAt = e.clock.Now()
	applyLatestJobViews(&next.scheduler)
	e.published.Store(&next)
}
