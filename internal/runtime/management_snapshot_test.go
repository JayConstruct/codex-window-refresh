package runtime

import (
	"context"
	"testing"

	"codex-window-refresh/internal/config"
)

func TestManagementSnapshotVersionCoversProgressAndAppliedConfig(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	initial := e.ManagementSnapshot()
	if initial.InstanceID == "" || initial.SnapshotSeq == 0 {
		t.Fatal("snapshot has no identity")
	}
	cfg := config.Defaults()
	businessEnabled := true
	cfg.BusinessEnabled, cfg.ManualWakeDisabled = &businessEnabled, true
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	configured := e.ManagementSnapshot()
	if configured.InstanceID != initial.InstanceID || configured.SnapshotSeq <= initial.SnapshotSeq || configured.State.Revision != initial.State.Revision {
		t.Fatal("configuration must advance the view version without inventing a durable transition")
	}
	if configured.Config.BusinessEnabled == nil || !*configured.Config.BusinessEnabled || !configured.Config.ManualWakeDisabled {
		t.Fatal("effective and business configuration switches lost")
	}
	status, id := e.QueueQuotaQueryJob("a")
	queued := e.ManagementSnapshot()
	if status != "queued" || id == "" || queued.SnapshotSeq <= configured.SnapshotSeq || queued.State.Revision != configured.State.Revision {
		t.Fatal("queue acceptance did not publish its own version")
	}
	if len(queued.Scheduler.Jobs) != 1 || queued.Scheduler.Jobs[0].JobID != id || queued.Scheduler.Jobs[0].Phase != "queued" {
		t.Fatal("accepted identity not observable before the worker runs")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished := e.ManagementSnapshot()
	if finished.SnapshotSeq <= queued.SnapshotSeq || finished.Scheduler.Jobs[0].Phase != "completed" {
		t.Fatal("query result did not advance the same instance")
	}
	other, _, _, _ := fixture(t, false, false)
	if other.ManagementSnapshot().InstanceID == initial.InstanceID {
		t.Fatal("different engine reused an instance identity")
	}
}

func TestManagementSnapshotReadersCannotMutatePublishedData(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	cfg := config.Defaults()
	business := true
	cfg.BusinessEnabled = &business
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, id := e.QueueQuotaQueryJob("a")
	first := e.ManagementSnapshot()
	delete(first.State.Accounts, "a")
	first.Config.Execution.PromptPool[0] = "changed"
	*first.Config.BusinessEnabled = false
	first.Scheduler.Jobs[0].QuotaQuery.Phase = "fake"
	first.Scheduler.HoldCounts["fake"] = 42
	next := e.ManagementSnapshot()
	if _, ok := next.State.Accounts["a"]; !ok || next.Config.Execution.PromptPool[0] == "changed" || !*next.Config.BusinessEnabled || next.Scheduler.Jobs[0].JobID != id || next.Scheduler.Jobs[0].QuotaQuery.Phase != "queued" || next.Scheduler.HoldCounts["fake"] != 0 {
		t.Fatal("snapshot caller mutated shared runtime data")
	}
}
