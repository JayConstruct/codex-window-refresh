package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-window-refresh/internal/core"
)

type diagnosticTestError struct {
	code   string
	detail core.QuotaDiagnostic
}

func (e diagnosticTestError) Error() string                         { return e.code }
func (e diagnosticTestError) QuotaDiagnostic() core.QuotaDiagnostic { return e.detail }

func TestManualQuotaDiagnosticSurvivesPublicationWithoutTrustingRemoteFields(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		e, h, _, _ := fixture(t, false, false)
		e.cfg.Enabled, e.cfg.QuotaQuery.Automatic = false, false
		err := diagnosticTestError{code: "quota_upstream_forbidden", detail: core.QuotaDiagnostic{Code: "private metadata", Stage: "upstream_http", HTTPStatus: 403, ElapsedMS: -1}}
		if unsafe {
			err.code = "private token"
			err.detail.Stage = "private proxy"
			err.detail.HTTPStatus = 999
		}
		e.host = controlledHost{Host: h, read: func(context.Context, core.QuotaRead) (core.QuotaObservation, error) {
			return core.QuotaObservation{}, err
		}}
		e.QueueQuotaQuery("a")
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		s := e.SchedulerDiagnostics().(SchedulingSummary)
		if s.QuotaQuery.Diagnostic == nil || s.LastQuotaDiagnostic == nil || len(h.sends) != 0 {
			t.Fatal("query lost diagnostic or dispatched a model")
		}
		d := s.QuotaQuery.Diagnostic
		if unsafe {
			if d.Code != "quota_read_failed" || d.Stage != "query" || d.HTTPStatus != 0 {
				t.Fatal("remote metadata bypassed allowlist")
			}
		} else if d.Code != "quota_upstream_forbidden" || d.Stage != "upstream_http" || d.HTTPStatus != 403 {
			t.Fatal("structured status was flattened")
		}
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), "private") || d.ElapsedMS < 0 {
			t.Fatal("public status contains private/invalid metadata")
		}
		s.QuotaQuery.Diagnostic.Stage = "private mutation"
		s.LastQuotaDiagnostic.Code = "private mutation"
		raw, _ = json.Marshal(e.SchedulerDiagnostics())
		if strings.Contains(string(raw), "private") {
			t.Fatal("status exposed mutable published pointers")
		}
	}
}
