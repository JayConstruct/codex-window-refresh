package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/events"
)

func trafficEvent(seq uint64, kind, id string, observation events.Traffic) events.Event {
	e := event(seq, kind, id, "caller-secret-material", observation.AccountID)
	e.Traffic = &observation
	return e
}
func number(v int64) *int64 { return &v }
func httpNumber(v int) *int { return &v }

func TestTrafficFailoverCorrelationReplayAndPrivacy(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	observed := batch(0,
		trafficEvent(1, "started", "inbound", events.Traffic{Phase: "started", Caller: "caller-secret-material", Session: "private-session", TraceID: "trace"}),
		trafficEvent(2, "traffic", "selection-a", events.Traffic{Phase: "decision", SelectionID: "selection-a", AccountID: "a", Caller: "caller-secret-material", Session: "private-session", Project: "project-a", Model: "fixture-model", Reason: "project_preference", Evidence: map[string]any{"inflight": float64(1), "api_key": "NEVER-PERSIST"}}),
		trafficEvent(3, "account_selected", "inbound", events.Traffic{Phase: "selected", SelectionID: "selection-a", AccountID: "a"}),
		trafficEvent(4, "traffic", "upstream-a", events.Traffic{Phase: "usage", SelectionID: "selection-a", ExecutionID: "upstream-a", AccountID: "a", Status: "failed", HTTPStatus: httpNumber(429)}),
		trafficEvent(5, "traffic", "selection-b", events.Traffic{Phase: "decision", SelectionID: "selection-b", AccountID: "b", Caller: "caller-secret-material", Project: "project-a", Session: "private-session", Reason: "failover"}),
		trafficEvent(6, "account_selected", "inbound", events.Traffic{Phase: "selected", SelectionID: "selection-b", AccountID: "b"}),
		trafficEvent(7, "traffic", "upstream-b", events.Traffic{Phase: "usage", SelectionID: "selection-b", ExecutionID: "upstream-b", AccountID: "b", Status: "completed", InputTokens: number(120), OutputTokens: number(30), LatencyNS: number(2e9)}),
		trafficEvent(8, "completed", "inbound", events.Traffic{Phase: "completed", SelectionID: "selection-b", AccountID: "b", Status: "succeeded"}),
	)
	if _, err := s.ApplyEvents(ctx, observed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEvents(ctx, observed); err != nil {
		t.Fatal(err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{Kind: "analysis"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary != (TrafficSummary{Requests: 1, Attempts: 2, Recovered: 1}) || len(p.Records) != 1 {
		t.Fatalf("counts: %+v", p)
	}
	r := p.Records[0]
	if r.AccountID != "b" || r.Project != "project-a" || r.Reason != "failover" || r.InputTokens == nil || *r.InputTokens != 120 || r.ReportedUsageAttempts != 1 {
		t.Fatalf("record: %+v", r)
	}
	detail, err := s.Traffic(ctx, TrafficFilter{ID: r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Record == nil || len(detail.Record.Attempts) != 2 || detail.Record.Attempts[0].InputTokens != nil || detail.Record.CachedTokens != nil {
		t.Fatalf("missing observations were manufactured: %+v", detail.Record)
	}
	for _, filter := range []TrafficFilter{{Account: "a"}, {Search: "project_preference"}, {Session: r.Session}} {
		p, err = s.Traffic(ctx, filter)
		if err != nil || p.Total != 1 {
			t.Fatalf("attempt-aware filter %+v: %+v %v", filter, p, err)
		}
	}
	var raw string
	if err = s.db.QueryRow("SELECT group_concat(data) FROM traffic_requests").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "caller-secret-material") || strings.Contains(raw, "private-session") {
		t.Fatal("raw identity persisted")
	}
	if err = s.db.QueryRow("SELECT group_concat(data) FROM traffic_attempts").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "NEVER-PERSIST") || strings.Contains(raw, "private-session") || strings.Contains(raw, "caller-secret-material") {
		t.Fatal("non-allowlisted metadata persisted", raw)
	}
	// Delivery replay can carry a fresh event sequence but the same execution ID.
	duplicate := trafficEvent(9, "traffic", "upstream-b", *observed.Events[6].Traffic)
	if _, err = s.ApplyEvents(ctx, batch(8, duplicate)); err != nil {
		t.Fatal(err)
	}
	p, err = s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Attempts != 2 {
		t.Fatal("execution replay double counted", p, err)
	}
}

func TestTrafficDistinctExecutionsCanShareSelectionAndUnknownSurvivesGap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	list := batch(0,
		trafficEvent(1, "started", "request", events.Traffic{Phase: "started", Caller: "scope"}),
		trafficEvent(2, "account_selected", "request", events.Traffic{Phase: "selected", SelectionID: "same-selection", AccountID: "a"}),
		trafficEvent(3, "traffic", "execution-one", events.Traffic{Phase: "usage", SelectionID: "same-selection", ExecutionID: "execution-one", AccountID: "a", Status: "failed"}),
		trafficEvent(4, "traffic", "execution-two", events.Traffic{Phase: "usage", SelectionID: "same-selection", ExecutionID: "execution-two", AccountID: "a", Status: "completed"}),
	)
	if _, err := s.ApplyEvents(ctx, list); err != nil {
		t.Fatal(err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Requests != 1 || p.Summary.Attempts != 2 {
		t.Fatal(p, err)
	}
	gap := batch(4)
	gap.Earliest = 6
	gap.Latest = 5
	gap.Gap = true
	gap.LossEpoch = 1
	if _, err = s.ApplyEvents(ctx, gap); err != nil {
		t.Fatal(err)
	}
	p, err = s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Unknown != 1 || p.Summary.Recovered != 0 || p.Coverage.Status != "gap" {
		t.Fatal("gap was hidden", p, err)
	}
}

func TestTrafficLateDecisionAndUnlinkedUsageNeverGuessRequests(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	list := batch(0,
		trafficEvent(1, "started", "r", events.Traffic{Phase: "started", Caller: "scope"}),
		trafficEvent(2, "account_selected", "r", events.Traffic{Phase: "selected", SelectionID: "sel", AccountID: "a"}),
		trafficEvent(3, "traffic", "exec", events.Traffic{Phase: "usage", SelectionID: "sel", ExecutionID: "exec", AccountID: "a", Status: "completed"}),
		trafficEvent(4, "traffic", "sel", events.Traffic{Phase: "decision", SelectionID: "sel", AccountID: "a", Reason: "parent_affinity", Caller: "scope", Session: "child", ParentSession: "parent"}),
		trafficEvent(5, "traffic", "unrelated", events.Traffic{Phase: "usage", ExecutionID: "unrelated", AccountID: "a", Status: "completed"}),
	)
	if _, err := s.ApplyEvents(ctx, list); err != nil {
		t.Fatal(err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Attempts != 1 || p.Summary.Requests != 1 || p.Records[0].Reason != "parent_affinity" {
		t.Fatal("linked by time or lost late decision", p, err)
	}
	if p.Records[0].Caller == "" || p.Records[0].Session == "" || p.Records[0].ParentSession == "" {
		t.Fatal("late metadata lost")
	}
	// Missing input/output usage is represented by JSON null, not a false zero.
	raw, _ := json.Marshal(p.Records[0])
	if !strings.Contains(string(raw), `"input_tokens":null`) {
		t.Fatal(string(raw))
	}
}

func TestTrafficRejectDoesNotManufactureAnExecutionAndRetentionIsIndependent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	list := batch(0, trafficEvent(1, "started", "blocked", events.Traffic{Phase: "started", Caller: "scope"}), trafficEvent(2, "account_selected", "blocked", events.Traffic{Phase: "selected", SelectionID: "s"}), trafficEvent(3, "traffic", "s", events.Traffic{Phase: "decision", SelectionID: "s", Reason: "scheduler_busy", Status: "not_sent"}), trafficEvent(4, "completed", "blocked", events.Traffic{Phase: "completed", Status: "failed"}))
	if _, err := s.ApplyEvents(ctx, list); err != nil {
		t.Fatal(err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Attempts != 0 || p.Records[0].Status != "not_sent" || p.Records[0].AccountID != "" {
		t.Fatal(p, err)
	}
	if err = s.Prune(ctx, time.Now().Add(8*24*time.Hour), DefaultRetention()); err != nil {
		t.Fatal(err)
	}
	p, err = s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Total != 0 {
		t.Fatal(p, err)
	}
}

func TestTrafficTraceFallbackIsUniqueAndAmbiguityRetractsIt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.ApplyEvents(ctx, batch(0,
		trafficEvent(1, "started", "first", events.Traffic{Phase: "started", Caller: "scope", TraceID: "collision"}),
		trafficEvent(2, "traffic", "execution", events.Traffic{Phase: "usage", ExecutionID: "execution", TraceID: "collision", AccountID: "a", Status: "completed"}),
	)); err != nil {
		t.Fatal(err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Attempts != 1 {
		t.Fatal(p, err)
	}
	if _, err = s.ApplyEvents(ctx, batch(2, trafficEvent(3, "started", "second", events.Traffic{Phase: "started", Caller: "scope", TraceID: "collision"}))); err != nil {
		t.Fatal(err)
	}
	p, err = s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Summary.Requests != 2 || p.Summary.Attempts != 0 || p.Coverage.UnlinkedAttempts != 1 {
		t.Fatal("ambiguous trace kept a guessed owner", p, err)
	}
}

func TestTrafficSchemaElevenMigrationPreservesExistingJobs(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := createTest(t, s, "retained", "account")
	for _, statement := range []string{"DROP TABLE traffic_attempts", "DROP TABLE traffic_requests", "DROP TABLE traffic_meta", "DROP TABLE traffic_operation_evidence", "UPDATE schema_version SET version=11"} {
		if _, err = s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restored, err := s.Job(context.Background(), job.ID)
	if err != nil || restored.ID != job.ID {
		t.Fatal(restored, err)
	}
	var version int
	if err = s.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 12 {
		t.Fatal(version, err)
	}
	p, err := s.Traffic(context.Background(), TrafficFilter{})
	if err != nil || p.Total != 0 {
		t.Fatal(p, err)
	}
}

func TestTrafficAggregateBoundsKeepFullTotalsAndExposeTruncation(t *testing.T) {
	p := TrafficPage{Summary: TrafficSummary{Requests: 999, Attempts: 1024}}
	for i := 0; i < 40; i++ {
		p.Accounts = append(p.Accounts, TrafficAccount{AccountID: string(rune('A' + i)), Attempts: 40 - i})
	}
	accounts := map[string]int{}
	for i := 0; i < 40; i++ {
		accounts[string(rune('A'+i))] = 40 - i
	}
	p.Projects = []TrafficProject{{Project: "project", Attempts: 820, Accounts: accounts}}
	boundTrafficPage(&p)
	if !p.AggregationLimited || len(p.Accounts) != 32 || len(p.Projects[0].Accounts) != 32 || p.Summary.Attempts != 1024 || p.Projects[0].Attempts != 820 {
		t.Fatal(p)
	}
}
