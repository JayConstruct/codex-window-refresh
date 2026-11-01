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

func TestTrafficCatalogCompletionDoesNotInventModelRequests(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// CPA emits these completions for model catalog reads without running a model.
	observed := batch(0,
		trafficEvent(1, "completed", "models-list", events.Traffic{Phase: "completed", TraceID: "catalog-trace", Status: "succeeded", HTTPStatus: httpNumber(200)}),
		trafficEvent(2, "completed", "model-detail", events.Traffic{Phase: "completed", Status: "failed", HTTPStatus: httpNumber(404)}),
	)
	if through, err := s.ApplyEvents(ctx, observed); err != nil || through != 2 {
		t.Fatal(through, err)
	}
	p, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || p.Total != 0 || p.Summary.Requests != 0 {
		t.Fatal("catalog reads counted as model requests", p, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM traffic_requests").Scan(&count); err != nil || count != 0 {
		t.Fatal("empty completions consumed request history capacity", count, err)
	}
	// An empty completion must still finish a previously observed user request.
	// A model request seen only at completion remains valid without a caller.
	observed = batch(2,
		trafficEvent(3, "started", "business", events.Traffic{Phase: "started", Caller: "scope"}),
		trafficEvent(4, "completed", "business", events.Traffic{Phase: "completed", Status: "succeeded"}),
		trafficEvent(5, "completed", "late-model", events.Traffic{Phase: "completed", Model: "fixture-model", Status: "failed"}),
		trafficEvent(6, "account_selected", "selected", events.Traffic{Phase: "selected", AccountID: "a", SelectionID: "selection"}),
		trafficEvent(7, "completed", "selected", events.Traffic{Phase: "completed", Status: "succeeded"}),
	)
	if _, err := s.ApplyEvents(ctx, observed); err != nil {
		t.Fatal(err)
	}
	p, err = s.Traffic(ctx, TrafficFilter{Kind: "analysis"})
	if err != nil || p.Total != 3 || p.Summary.Attempts != 0 {
		t.Fatal("valid requests without usage or caller were lost", p, err)
	}
	for _, r := range p.Records {
		if (r.RequestID == "business" || r.RequestID == "selected") && r.Status != "completed" {
			t.Fatal("empty completion did not finish known request", r)
		}
	}
}

func TestTrafficLegacyEmptyCompletionsStayOutsideUserHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	stamp := now()
	for _, id := range []string{"legacy-catalog", "legacy-execution", "legacy-selection"} {
		r := TrafficRecord{ID: trafficHash("bridge-a", "request", id), RequestID: id, Status: "completed", CreatedAt: stamp, UpdatedAt: stamp, Reason: "unknown"}
		if _, err := s.db.Exec(`INSERT INTO traffic_requests(id,bridge_instance,request_id,trace_id,status,created_at,updated_at,data) VALUES(?,?,?,?,?,?,?,?)`, r.ID, "bridge-a", id, "trace-"+id, r.Status, stamp, stamp, mustTrafficJSON(r)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"legacy-execution", "legacy-selection"} {
		a := TrafficAttempt{ID: "attempt-" + id, Executed: id == "legacy-execution", Status: "completed", Reason: "unknown"}
		if _, err := s.db.Exec(`INSERT INTO traffic_attempts(id,bridge_instance,request_id,executed,created_at,updated_at,data) VALUES(?,?,?,?,?,?,?)`, a.ID, "bridge-a", trafficHash("bridge-a", "request", id), a.Executed, stamp, stamp, mustTrafficJSON(a)); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"users", "analysis"} {
		p, err := s.Traffic(ctx, TrafficFilter{Kind: kind})
		if err != nil || p.Total != 2 || p.Summary.Attempts != 1 || p.Coverage.UnclassifiedRequests != 1 {
			t.Fatal("legacy blank completion inflated user statistics or execution evidence was lost", p, err)
		}
		for _, r := range p.Records {
			if r.RequestID == "legacy-catalog" {
				t.Fatal("legacy catalog completion still shown as a model request")
			}
		}
	}
	if _, err := s.Traffic(ctx, TrafficFilter{ID: "legacy-catalog"}); err != ErrNotFound {
		t.Fatal("excluded completion remains accessible as a user request", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM traffic_requests").Scan(&count); err != nil || count != 3 {
		t.Fatal("history reads deleted stored evidence", count, err)
	}
}

func TestResolveTrafficCallersMatchesStoredIdentityWithoutWriting(t *testing.T) {
	s, other := openTest(t), openTest(t)
	ctx := context.Background()
	scope := trafficHash("cli-proxy-api:caller-scope:v1", "fixture-key")
	if _, err := s.ApplyEvents(ctx, batch(0, trafficEvent(1, "started", "r", events.Traffic{Phase: "started", Caller: scope}))); err != nil {
		t.Fatal(err)
	}
	page, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || len(page.Records) != 1 {
		t.Fatal(page, err)
	}
	var before, after int
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	callers, err := s.ResolveTrafficCallers(ctx, []string{scope, scope})
	if err != nil || len(callers) != 2 || callers[0] != page.Records[0].Caller || callers[1] != callers[0] {
		t.Fatal(callers, err)
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || before != after {
		t.Fatal("lookup wrote to database", before, after, err)
	}
	isolated, err := other.ResolveTrafficCallers(ctx, []string{scope})
	if err != nil || isolated[0] == callers[0] {
		t.Fatal("caller handles must stay database scoped", isolated, err)
	}
	for _, scopes := range [][]string{nil, {}, {"fixture-key"}, {strings.ToUpper(scope)}, {scope[:63]}, make([]string, 65)} {
		if _, err := s.ResolveTrafficCallers(ctx, scopes); err != ErrTrafficFilter {
			t.Fatal("invalid scope accepted", err)
		}
	}
}

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

func TestTrafficRunningRequestShowsCurrentSelectionBeforeUsage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	apply := func(after uint64, observed ...events.Event) {
		t.Helper()
		if _, err := s.ApplyEvents(ctx, batch(after, observed...)); err != nil {
			t.Fatal(err)
		}
	}
	read := func(account, reason string, attempts int) TrafficRecord {
		t.Helper()
		page, err := s.Traffic(ctx, TrafficFilter{Kind: "users", Account: account})
		if err != nil || page.Total != 1 || len(page.Records) != 1 {
			t.Fatalf("running account filter: %+v %v", page, err)
		}
		r := page.Records[0]
		if r.Status != "running" || r.AccountID != account || r.Reason != reason || r.AttemptCount != attempts || page.Summary.Attempts != attempts {
			t.Fatalf("current selection lost or counted as execution: %+v", r)
		}
		detail, err := s.Traffic(ctx, TrafficFilter{ID: r.ID})
		if err != nil || detail.Record == nil || detail.Record.AccountID != account || detail.Record.Reason != reason {
			t.Fatalf("detail lost current selection: %+v %v", detail.Record, err)
		}
		return r
	}
	apply(0,
		trafficEvent(1, "started", "live", events.Traffic{Phase: "started", Caller: "scope"}),
		trafficEvent(2, "traffic", "sel-a", events.Traffic{Phase: "decision", SelectionID: "sel-a", AccountID: "a", Reason: "project_preference", Caller: "scope"}),
		trafficEvent(3, "account_selected", "live", events.Traffic{Phase: "selected", SelectionID: "sel-a", AccountID: "a"}),
	)
	r := read("a", "project_preference", 0)
	if r.Attempted || r.InputTokens != nil || r.OutputTokens != nil || r.LatencyMS != nil {
		t.Fatalf("selection manufactured execution or usage: %+v", r)
	}
	analysis, err := s.Traffic(ctx, TrafficFilter{Kind: "analysis"})
	if err != nil || analysis.Summary.Attempts != 0 || len(analysis.Accounts) != 0 {
		t.Fatalf("selected account counted as executed: %+v %v", analysis, err)
	}
	apply(3,
		trafficEvent(4, "traffic", "sel-b", events.Traffic{Phase: "decision", SelectionID: "sel-b", AccountID: "b", Reason: "failover", Caller: "scope"}),
		trafficEvent(5, "account_selected", "live", events.Traffic{Phase: "selected", SelectionID: "sel-b", AccountID: "b"}),
		// An earlier account can report its outcome after the retry is selected.
		trafficEvent(6, "traffic", "exec-a", events.Traffic{Phase: "usage", SelectionID: "sel-a", ExecutionID: "exec-a", AccountID: "a", Status: "failed", HTTPStatus: httpNumber(429)}),
	)
	read("b", "failover", 1)
	apply(6,
		// Reusing the same account must still use this selection's reason.
		trafficEvent(7, "account_selected", "live", events.Traffic{Phase: "selected", SelectionID: "sel-b2", AccountID: "b"}),
	)
	read("b", "unknown", 1)
	apply(7, trafficEvent(8, "traffic", "sel-b2", events.Traffic{Phase: "decision", SelectionID: "sel-b2", AccountID: "b", Reason: "session_affinity", Caller: "scope"}))
	read("b", "session_affinity", 1)
	apply(8,
		trafficEvent(9, "traffic", "exec-b", events.Traffic{Phase: "usage", SelectionID: "sel-b2", ExecutionID: "exec-b", AccountID: "b", Status: "completed", InputTokens: number(120)}),
		trafficEvent(10, "completed", "live", events.Traffic{Phase: "completed", SelectionID: "sel-b2", AccountID: "b", Status: "succeeded"}),
	)
	page, err := s.Traffic(ctx, TrafficFilter{ID: r.ID})
	if err != nil || page.Record == nil || page.Record.Status != "completed" || page.Record.AccountID != "b" || page.Record.AttemptCount != 2 || !page.Record.Recovered {
		t.Fatalf("final execution lost after live selection: %+v %v", page.Record, err)
	}
}

func TestTrafficCompletedSelectionWithoutUsageDoesNotInventExecution(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.ApplyEvents(ctx, batch(0,
		trafficEvent(1, "started", "selected-only", events.Traffic{Phase: "started", Caller: "scope"}),
		trafficEvent(2, "account_selected", "selected-only", events.Traffic{Phase: "selected", SelectionID: "sel", AccountID: "a"}),
		trafficEvent(3, "completed", "selected-only", events.Traffic{Phase: "completed", Status: "succeeded"}),
	)); err != nil {
		t.Fatal(err)
	}
	page, err := s.Traffic(ctx, TrafficFilter{})
	if err != nil || page.Total != 1 || page.Records[0].AccountID != "" || page.Summary.Attempts != 0 || page.Records[0].Attempted {
		t.Fatalf("completion without execution invented final account: %+v %v", page, err)
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
