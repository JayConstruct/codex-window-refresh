package plugin

import (
	"os"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	scheduler "codex-window-refresh/internal/runtime"
)

type capabilityView struct {
	core.HostCapabilities
	QuotaBridge          quotaBridgeDiagnostic     `json:"quota_bridge"`
	InventoryDiagnostics map[core.AccountID]string `json:"account_inventory_diagnostics,omitempty"`
}

func schedulerRunning(done <-chan struct{}, stopping bool) bool {
	if stopping || done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

func snapshotCapabilities(snapshot scheduler.ManagementSnapshot, wrapper WrapperConfig) capabilityView {
	caps := snapshot.Capabilities
	view := capabilityView{HostCapabilities: caps, QuotaBridge: quotaBridgeDiagnostic{Mode: wrapper.quotaTransport()}}
	if view.QuotaBridge.Mode == "host" {
		view.QuotaBridge.Status = "host_callback"
		view.QuotaBridge.HostHTTPStatus = snapshot.HostHTTPStatus
	} else {
		// Configuration diagnostics contain no credential values.
		bridge := &cpa.BridgeReader{ManagementURL: wrapper.ManagementURL, ManagementKeyEnv: wrapper.ManagementKeyEnv, Getenv: os.Getenv}
		view.QuotaBridge.Status, view.QuotaBridge.ManagementKeyEnv = bridge.ConfigurationStatus(), wrapper.ManagementKeyEnv
	}
	view.InventoryDiagnostics = snapshot.InventoryDiagnostics
	return view
}

func (s *Service) snapshotResponse(engine Engine, wrapper WrapperConfig, done <-chan struct{}, stopping bool, lastError string) managementResponse {
	reader, ok := engine.(interface {
		ManagementSnapshot() scheduler.ManagementSnapshot
	})
	if !ok {
		return response(501, map[string]string{"error": "snapshot_unavailable"})
	}
	view := reader.ManagementSnapshot()
	if view.InstanceID == "" || view.SnapshotSeq == 0 {
		return response(503, map[string]string{"error": "snapshot_unavailable"})
	}
	if view.LastError != "" {
		lastError = view.LastError
	}
	if view.Scheduler.Jobs == nil {
		view.Scheduler.Jobs = []scheduler.JobStatus{}
	}
	if view.State.Accounts == nil {
		view.State.Accounts = map[core.AccountID]core.AccountState{}
	}
	businessEnabled := view.Config.Enabled
	if view.Config.BusinessEnabled != nil {
		businessEnabled = *view.Config.BusinessEnabled
	}
	return response(200, map[string]any{
		"instance_id": view.InstanceID, "snapshot_seq": view.SnapshotSeq,
		"summary": map[string]any{
			"enabled": view.Config.Enabled, "business_enabled": businessEnabled,
			"host_enabled": !view.Config.ManualWakeDisabled, "running": schedulerRunning(done, stopping),
			"revision": view.State.Revision, "accounts": len(view.State.Accounts),
			"reservations": len(view.State.Reservations), "last_error": lastError,
			"effective_concurrency": 1, "scheduler": view.Scheduler,
		},
		"accounts":     view.State.Accounts,
		"capabilities": snapshotCapabilities(view, wrapper),
		"config":       view.Config,
	})
}
