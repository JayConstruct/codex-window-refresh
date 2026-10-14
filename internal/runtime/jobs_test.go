package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func findJob(t *testing.T, e *Engine, id string) JobStatus {
	t.Helper()
	for _, job := range e.JobStatuses() {
		if job.JobID == id {
			return job
		}
	}
	t.Fatalf("job %s missing", id)
	return JobStatus{}
}

func TestAcceptedJobIdentityIsAtomicAndResultsSurviveOtherTabs(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	var workers sync.WaitGroup
	ids := make(chan string, 32)
	for index := 0; index < cap(ids); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			status, id := e.QueueQuotaQueryJob("a")
			if status != "queued" && status != "already_queued" {
				t.Errorf("queue: %s", status)
			}
			ids <- id
		}()
	}
	workers.Wait()
	close(ids)
	var original string
	for id := range ids {
		if id == "" || (original != "" && original != id) {
			t.Fatal("coalesced clicks received different job identities")
		}
		original = id
	}
	if job := findJob(t, e, original); job.Phase != "queued" {
		t.Fatal("accepted job missing before scheduler runs")
	}
	view := e.ManagementSnapshot()
	if len(view.Scheduler.Jobs) != 1 || view.Scheduler.Jobs[0].JobID != original {
		t.Fatal("accepted identity missing from atomic panel snapshot")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, next := e.QueueQuotaQueryJob("removed")
	if status != "queued" || next == original {
		t.Fatal("subsequent job reused identity")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, e, original)
	if job.Phase != "completed" || job.QuotaQuery.Completed != 1 {
		t.Fatalf("another tab displaced the result: %+v", job)
	}
	job.QuotaQuery.Completed = 999
	if findJob(t, e, original).QuotaQuery.Completed != 1 {
		t.Fatal("reader mutated retained job")
	}
}

func TestJobProgressPublishesDuringReadWithoutPendingAccountEvidence(t *testing.T) {
	e, host, _, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	host.initial = append(host.initial, core.HostAccount{ID: "b", Generation: "other", Enabled: true})
	entered, release := make(chan struct{}), make(chan struct{})
	e.host = controlledHost{Host: host, read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		if read.AccountID == "b" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return core.QuotaObservation{}, ctx.Err()
			}
		}
		return host.observation(read.AccountID, read.Generation, true), nil
	}}
	_, id := e.QueueQuotaQueryJob("")
	done := make(chan error, 1)
	go func() { done <- e.Step(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second read did not start")
	}
	job := findJob(t, e, id)
	if job.Phase != "running" || job.QuotaQuery.Completed != 1 || job.QuotaQuery.Total != 2 {
		t.Errorf("progress not published: %+v", job)
	}
	if e.Snapshot().Accounts["a"].FiveHour.State == core.WindowActive {
		t.Error("uncommitted quota evidence published")
	}
	// Acceptance of a later request must remain observable if the scheduler is
	// occupied by a network call. This coalesced request also exercises the
	// publication lock while Step holds the execution mutex.
	if status, accepted := e.QueueQuotaQueryJob(""); status != "already_queued" || accepted != id {
		t.Error("running job failed to coalesce")
	}
	view := e.ManagementSnapshot()
	if len(view.Scheduler.Jobs) != 1 || view.Scheduler.Jobs[0].JobID != id {
		t.Error("accepted job absent while worker was blocked")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if findJob(t, e, id).Phase != "completed" || e.Snapshot().Accounts["a"].FiveHour.State != core.WindowActive {
		t.Fatal("committed completion was not published")
	}
}

type blockedInventoryHost struct {
	core.Host
	capability bool
}

func (h blockedInventoryHost) Capabilities(ctx context.Context) (core.HostCapabilities, error) {
	if h.capability {
		return core.HostCapabilities{}, errors.New("private capabilities failure")
	}
	return h.Host.Capabilities(ctx)
}
func (h blockedInventoryHost) Accounts(context.Context) ([]core.HostAccount, error) {
	return nil, errors.New("private inventory failure")
}

func TestBlockedJobsPublishReasonAndEventuallyReleaseSlot(t *testing.T) {
	for _, capability := range []bool{false, true} {
		t.Run(fmt.Sprint(capability), func(t *testing.T) {
			e, host, _, clock := fixture(t, false, false)
			e.host = blockedInventoryHost{Host: host, capability: capability}
			_, id := e.QueueQuotaQueryJob("a")
			if err := e.Step(context.Background()); err == nil {
				t.Fatal("expected host read failure")
			}
			job := findJob(t, e, id)
			if job.Phase != "queued" || job.BlockedReason == "" || job.RetryAt.IsZero() {
				t.Fatalf("blocked job looks indefinitely queued: %+v", job)
			}
			clock.now = clock.now.Add(manualJobBlockLimit)
			_ = e.Step(context.Background())
			job = findJob(t, e, id)
			if job.Phase != "failed" || job.QuotaQuery.LastError == "" || job.BlockedReason != "" || !job.RetryAt.IsZero() {
				t.Fatalf("blocked job never terminated: %+v", job)
			}
			if status, _ := e.QueueQuotaQueryJob("other"); status != "queued" {
				t.Fatal("blocked terminal job retained busy slot")
			}
		})
	}
}

type rejectingJobStore struct{ core.Store }

func (rejectingJobStore) Commit(context.Context, uint64, core.CoreState) (uint64, error) {
	return 0, errors.New("private disk error")
}

func TestFailedCommitCannotAdvertiseSuccessfulJobOrNewWork(t *testing.T) {
	e, _, store, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	before := e.Snapshot()
	e.store = rejectingJobStore{store}
	_, id := e.QueueQuotaQueryJob("a")
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("expected commit failure")
	}
	job := findJob(t, e, id)
	if job.Phase != "failed" || job.QuotaQuery.LastError != "durable_commit_failed" {
		t.Fatalf("uncommitted success leaked: %+v", job)
	}
	if e.Snapshot().Revision != before.Revision {
		t.Fatal("failed commit changed public revision")
	}
	if status, _ := e.QueueQuotaQueryJob("a"); status != "durable_commit_failed" {
		t.Fatalf("poisoned engine accepted new operation: %s", status)
	}
}

func TestWakeDispatchCommitFailurePublishesTerminalFailure(t *testing.T) {
	e, host, store, _ := manualWakeFixture(t)
	store.failDispatch = true
	_, id := e.QueueManualWakeJob("a")
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("expected failed durable dispatch")
	}
	job := findJob(t, e, id)
	if job.Phase != "failed" || job.ManualWake.LastError != "durable_commit_failed" {
		t.Fatalf("failed dispatch got stuck running: %+v", job)
	}
	if len(host.sends) != 0 {
		t.Fatal("model called without committed dispatch")
	}
}

func TestWakeAdmissionBlockIsTerminalSkippedJob(t *testing.T) {
	e, host, _, _ := manualWakeFixture(t)
	e.cfg.Execution.Model = ""
	_, id := e.QueueManualWakeJob("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, e, id)
	if job.Phase != "skipped" || job.ManualWake.Phase != "skipped" || job.ManualWake.LastError != "model_required" {
		t.Fatalf("terminal admission rejection appears active: %+v", job)
	}
	if len(host.sends) != 0 || e.manualWake.Phase != "blocked" {
		t.Fatal("admission rejection changed delivery behavior")
	}
}

func TestGenerationResultsRetainedAcrossNewRequestAndIdempotentReplay(t *testing.T) {
	e, host, _, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	host.active = true
	calls := 0
	e.host = generatingHost{Host: host, generate: func(context.Context, core.ExactRequest, int) (core.SendResult, []string, error) {
		calls++
		return core.SendResult{Disposition: core.SendCompleted}, generationCandidates(), nil
	}}
	first := generationRequest()
	if e.QueuePromptGeneration(first) != "queued" {
		t.Fatal("queue first generation")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RequestID = "test-generation-002"
	// Avoid the normal dispatch spacing policy for the second independent request.
	e.state.LastDispatchAt = time.Time{}
	e.publish(true)
	if e.QueuePromptGeneration(second) != "queued" {
		t.Fatal("queue second generation")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, e, first.RequestID)
	if job.Phase != "completed" || len(job.PromptGeneration.Candidates) != 5 {
		t.Fatal("new generation displaced first result")
	}
	if e.QueuePromptGeneration(first) != "already_queued" || calls != 2 {
		t.Fatal("older generation replay caused another model send")
	}
}

func TestRetainedJobsAreBounded(t *testing.T) {
	e, _, _, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	for index := 0; index < retainedJobLimit+5; index++ {
		if status, _ := e.QueueQuotaQueryJob("a"); status != "queued" {
			t.Fatal(status)
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if jobs := e.JobStatuses(); len(jobs) != retainedJobLimit {
		t.Fatalf("unbounded retained jobs: %d", len(jobs))
	}
}

func TestWakeResultSurvivesSubsequentWakeOnAnotherAccount(t *testing.T) {
	e, _, _, clock := manualWakeFixture(t)
	_, first := e.QueueManualWakeJob("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Minute)
	status, second := e.QueueManualWakeJob("b")
	if status != "queued" || first == second {
		t.Fatal("second wake identity missing")
	}
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if findJob(t, e, first).Phase != "completed" || findJob(t, e, second).Phase != "completed" {
		t.Fatal("retained wake result lost verification")
	}
}

func TestUnknownWakeJobStopsSpinnerWithoutResendingAndCanVerifyLater(t *testing.T) {
	e, host, _, clock := manualWakeFixture(t)
	host.unknown = true
	_, id := e.QueueManualWakeJob("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := findJob(t, e, id)
	if job.Phase != "unknown" || job.ManualWake.Phase != "unknown" || job.ManualWake.LastError != "send_failed" {
		t.Fatalf("uncertain send not explicitly surfaced: %+v", job)
	}
	if e.manualWake.Phase != "verifying" || e.QueueManualWake("a") != "delivery_evidence_pending" {
		t.Fatal("uncertain delivery lost retry suppression")
	}
	clock.now = clock.now.Add(time.Second)
	observation := host.observation("a", "g", true)
	observation.Class = core.ObservationExecution
	e.EnqueueObservation(observation)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if findJob(t, e, id).Phase != "completed" || len(host.sends) != 1 {
		t.Fatal("later evidence failed to verify uncertain send or repeated it")
	}
}

func TestBlockedModelJobsAlsoReleaseWithoutSending(t *testing.T) {
	for _, kind := range []string{"wake", "generation"} {
		t.Run(kind, func(t *testing.T) {
			e, host, _, clock := fixture(t, false, false)
			e.host = generatingHost{Host: blockedInventoryHost{Host: host}, generate: func(context.Context, core.ExactRequest, int) (core.SendResult, []string, error) {
				t.Error("blocked generation dispatched")
				return core.SendResult{}, nil, nil
			}}
			var id string
			if kind == "wake" {
				status, accepted := e.QueueManualWakeJob("a")
				if status != "queued" {
					t.Fatal(status)
				}
				id = accepted
			} else {
				request := generationRequest()
				id = request.RequestID
				if status := e.QueuePromptGeneration(request); status != "queued" {
					t.Fatal(status)
				}
			}
			_ = e.Step(context.Background())
			if job := findJob(t, e, id); job.BlockedReason != "account_read_failed" || job.RetryAt.IsZero() {
				t.Fatalf("missing blockage: %+v", job)
			}
			clock.now = clock.now.Add(manualJobBlockLimit)
			_ = e.Step(context.Background())
			if findJob(t, e, id).Phase != "failed" {
				t.Fatal("blocked model job never terminated")
			}
			if len(host.sends) != 0 {
				t.Fatal("blocked wake dispatched")
			}
			if e.QueueQuotaQuery("a") != "queued" {
				t.Fatal("blocked model job kept the slot busy")
			}
		})
	}
}

func TestNewQueueAcceptancePublishesWhileAutomaticWorkerIsBlocked(t *testing.T) {
	e, host, _, _ := fixture(t, false, false)
	e.cfg.Enabled = false
	entered, release := make(chan struct{}), make(chan struct{})
	e.host = controlledHost{Host: host, read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return core.QuotaObservation{}, ctx.Err()
		}
		return host.observation(read.AccountID, read.Generation, false), nil
	}}
	done := make(chan error, 1)
	go func() { done <- e.Step(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic read did not start")
	}
	accepted := make(chan string, 1)
	go func() {
		status, id := e.QueueQuotaQueryJob("a")
		if status != "queued" {
			t.Errorf("queue status %s", status)
		}
		accepted <- id
	}()
	select {
	case id := <-accepted:
		view := e.ManagementSnapshot()
		if len(view.Scheduler.Jobs) != 1 || view.Scheduler.Jobs[0].JobID != id || view.Scheduler.Jobs[0].Phase != "queued" {
			t.Error("accepted job absent until network completes")
		}
	case <-time.After(time.Second):
		t.Error("queue publication waited for worker network")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
