package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
)

// Exercise the real CPA adapter and engine together: status must come from CPA,
// independently of the upstream quota result and without another pool scan.
type quotaStatusCPA struct {
	status, upstream string
	lists, reads     int
	modelCalls       int
	afterRead        bool
	failStatus       bool
	readErr          error
	onRead           func()
}

func (h *quotaStatusCPA) Call(_ context.Context, method string, _, response any) error {
	var value any
	switch method {
	case "host.auth.list":
		h.lists++
		value = map[string]any{"files": []any{map[string]any{"id": "a", "auth_index": "idx-a", "provider": "codex"}}}
	case "host.auth.get_runtime":
		if h.afterRead && h.failStatus {
			return errors.New("local status unavailable")
		}
		value = map[string]any{"auth": map[string]any{"id": "a", "auth_index": "idx-a", "provider": "codex", "status": "active", "disabled": h.status == core.AccountDisabled, "unavailable": h.status == core.AccountUnavailable}}
	case "host.auth.get":
		value = map[string]any{"auth_index": "idx-a", "json": map[string]any{"type": "codex", "account_id": h.upstream}}
	default:
		h.modelCalls++
		return fmt.Errorf("unexpected callback: %s", method)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, response)
}

func (h *quotaStatusCPA) Read(context.Context, cpa.AuthBinding) ([]byte, error) {
	h.reads++
	h.afterRead = true
	if h.onRead != nil {
		h.onRead()
	}
	return []byte(`{"rate_limit":{"allowed":true}}`), h.readErr
}

func quotaStatusFixture(t *testing.T) (*Engine, *quotaStatusCPA, *memoryStore) {
	t.Helper()
	e, _, store, _ := fixture(t, false, false)
	e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
	host := &quotaStatusCPA{status: core.AccountAvailable, upstream: "upstream-a"}
	e.host = cpa.NewHost(host, host)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A failed read must preserve earlier evidence rather than mark it fresh.
	a := e.state.Accounts["a"]
	a.FiveHour = core.WindowEvidence{State: core.WindowActive, ConfirmedAt: e.clock.Now(), ResetAt: e.clock.Now().Add(1), Remaining: &core.QuotaAmount{Value: 67}}
	e.state.Accounts["a"] = a
	if err := e.commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.lists = 0
	return e, host, store
}

func TestQuotaQueriesPublishCPAStatusAlongsideQuota(t *testing.T) {
	for _, path := range []string{"selected", "batch", "automatic"} {
		for _, status := range []string{core.AccountDisabled, core.AccountUnavailable, core.AccountAvailable} {
			for _, failed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/failure=%t", path, status, failed), func(t *testing.T) {
					e, host, _ := quotaStatusFixture(t)
					before := e.Snapshot().Accounts["a"].FiveHour
					if status == core.AccountAvailable {
						// Recovery must follow CPA, even if CPA was unavailable before
						// the query and the automatic path still has an enabled cache.
						host.status = core.AccountUnavailable
					}
					host.onRead = func() { host.status = status }
					if failed {
						host.readErr = cpa.ErrQuotaRateLimited
					}
					switch path {
					case "selected":
						e.QueueQuotaQuery("a")
					case "batch":
						e.QueueQuotaQuery("")
					case "automatic":
						e.cfg.QuotaQuery.Automatic = true
					}
					if err := e.Step(context.Background()); err != nil {
						t.Fatal(err)
					}
					a := e.Snapshot().Accounts["a"]
					if a.Availability != status || a.Enabled != (status == core.AccountAvailable) {
						t.Fatalf("query did not publish CPA status: %+v", a)
					}
					if failed {
						if a.FiveHour.Remaining == nil || a.FiveHour.Remaining.Value != 67 || a.FiveHour.ConfirmedAt != before.ConfirmedAt {
							t.Fatal("failed quota read replaced prior evidence")
						}
					} else if a.Permission != core.PermissionAllowed {
						t.Fatal("successful quota read was discarded after CPA status change")
					}
					wantLists := 0
					if path == "batch" {
						wantLists = 1
					}
					if host.lists != wantLists || host.reads != 1 || host.modelCalls != 0 {
						t.Fatalf("unexpected extra work: lists=%d reads=%d model=%d", host.lists, host.reads, host.modelCalls)
					}
					if path != "automatic" && (e.manualQuery.Completed != boolCount(!failed) || e.manualQuery.Failed != boolCount(failed)) {
						t.Fatalf("status update changed quota task outcome: %+v", e.manualQuery)
					}
				})
			}
		}
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestQuotaStatusRefreshRejectsReplacedCredentialEvidence(t *testing.T) {
	e, host, _ := quotaStatusFixture(t)
	before := e.Snapshot().Accounts["a"].Generation
	host.onRead = func() { host.upstream = "replacement" }
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := e.Snapshot().Accounts["a"]
	if a.Generation == before || a.FiveHour.Remaining != nil || a.Permission != core.PermissionUnknown || e.manualQuery.Failed != 1 {
		t.Fatalf("changed binding retained or accepted old evidence: %+v, %+v", a, e.manualQuery)
	}
	// The next query can use the accepted new identity immediately.
	host.onRead = nil
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.manualQuery.Completed != 1 || host.lists != 0 || host.reads != 2 || host.modelCalls != 0 {
		t.Fatalf("replacement required a pool scan or reused stale identity: %+v", e.manualQuery)
	}
}

func TestQuotaStatusReadFailurePreservesValidatedStatusAndQuotaError(t *testing.T) {
	e, host, _ := quotaStatusFixture(t)
	host.status = core.AccountDisabled
	host.readErr, host.failStatus = cpa.ErrQuotaRateLimited, true
	host.onRead = func() { host.status = core.AccountAvailable }
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := e.Snapshot().Accounts["a"]
	if a.Availability != core.AccountDisabled || a.Enabled || e.manualQuery.LastError != cpa.ErrQuotaRateLimited.Error() {
		t.Fatalf("unverified recovery or lost quota error: %+v, %+v", a, e.manualQuery)
	}
}

func TestQuotaStatusNotPublishedBeforeDurableCommit(t *testing.T) {
	e, host, store := quotaStatusFixture(t)
	before := e.Snapshot().Accounts["a"]
	e.store = rejectingJobStore{store}
	host.onRead = func() { host.status = core.AccountDisabled }
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err == nil {
		t.Fatal("expected commit rejection")
	}
	a := e.Snapshot().Accounts["a"]
	if a.Availability != before.Availability || a.Enabled != before.Enabled || a.Permission != before.Permission || host.modelCalls != 0 {
		t.Fatalf("uncommitted status leaked into published snapshot: %+v", a)
	}
}

func TestQuotaStatusRefreshPreservesPauseAndOtherAccounts(t *testing.T) {
	e, host, _ := quotaStatusFixture(t)
	paused := true
	e.cfg.AccountOverrides["a"] = config.AccountOverride{Paused: &paused}
	// Another account can have newer state than its cached inventory entry.
	// A targeted quota refresh must neither restore that stale status nor remove it.
	e.state.Accounts["b"] = core.AccountState{ID: "b", Generation: "other", Availability: core.AccountDisabled}
	e.inventory = append(e.inventory, core.HostAccount{ID: "b", Generation: "other", Enabled: true, Availability: core.AccountAvailable})
	e.QueueQuotaQuery("a")
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot()
	if s.Accounts["a"].Availability != core.AccountAvailable || s.Accounts["a"].Enabled || s.Accounts["a"].Permission != core.PermissionAllowed {
		t.Fatalf("CPA availability bypassed local pause or prevented quota read: %+v", s.Accounts["a"])
	}
	if s.Accounts["b"].Availability != core.AccountDisabled || s.Accounts["b"].Enabled || host.lists != 0 {
		t.Fatalf("selected quota refresh changed another account: %+v", s.Accounts["b"])
	}
}
