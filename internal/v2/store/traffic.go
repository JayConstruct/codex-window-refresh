package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"codex-window-refresh/internal/v2/events"
)

var ErrTrafficFilter = errors.New("invalid_traffic_filter")

type TrafficFilter struct {
	Kind, ID, Search, Project, Caller, Account, Model, Status, From, To, Session string
	Page, Limit                                                                  int
}

type TrafficAttempt struct {
	ID            string         `json:"id"`
	SelectionID   string         `json:"selection_id"`
	ExecutionID   string         `json:"execution_id"`
	AccountID     string         `json:"account_id"`
	Model         string         `json:"model"`
	Provider      string         `json:"provider"`
	Reason        string         `json:"reason"`
	Status        string         `json:"status"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
	HTTPStatus    *int           `json:"http_status"`
	LatencyMS     *float64       `json:"latency_ms"`
	TTFTMS        *float64       `json:"ttft_ms"`
	InputTokens   *int64         `json:"input_tokens"`
	OutputTokens  *int64         `json:"output_tokens"`
	CachedTokens  *int64         `json:"cached_tokens"`
	Evidence      map[string]any `json:"evidence"`
	Executed      bool           `json:"executed"`
	Caller        string         `json:"caller"`
	Session       string         `json:"session"`
	ParentSession string         `json:"parent_session"`
	Project       string         `json:"project"`
}

type TrafficRecord struct {
	ID                    string           `json:"id"`
	RequestID             string           `json:"request_id"`
	Project               string           `json:"project"`
	ProjectName           string           `json:"project_name"`
	Caller                string           `json:"caller"`
	Session               string           `json:"session"`
	ParentSession         string           `json:"parent_session"`
	Model                 string           `json:"model"`
	AccountID             string           `json:"account_id"`
	Reason                string           `json:"reason"`
	Status                string           `json:"status"`
	CreatedAt             string           `json:"created_at"`
	UpdatedAt             string           `json:"updated_at"`
	AttemptCount          int              `json:"attempt_count"`
	Recovered             bool             `json:"recovered"`
	LatencyMS             *float64         `json:"latency_ms"`
	TTFTMS                *float64         `json:"ttft_ms"`
	InputTokens           *int64           `json:"input_tokens"`
	OutputTokens          *int64           `json:"output_tokens"`
	CachedTokens          *int64           `json:"cached_tokens"`
	ReportedUsageAttempts int              `json:"reported_usage_attempts"`
	Attempts              []TrafficAttempt `json:"attempts,omitempty"`
	Kind                  string           `json:"kind,omitempty"`
	Trigger               string           `json:"trigger,omitempty"`
	Stage                 string           `json:"stage,omitempty"`
	HTTPStatus            *int             `json:"http_status"`
	Response              *WakeResponse    `json:"response,omitempty"`
	TaskID                string           `json:"task_id,omitempty"`
	TaskIDs               []string         `json:"task_ids,omitempty"`
	Attempted             bool             `json:"attempted"`
	Evidence              map[string]any   `json:"evidence,omitempty"`
}

type TrafficSummary struct {
	Requests  int `json:"requests"`
	Attempts  int `json:"attempts"`
	Recovered int `json:"recovered"`
	Unknown   int `json:"unknown"`
}
type TrafficAccount struct {
	AccountID             string `json:"account_id"`
	Attempts              int    `json:"attempts"`
	InputTokens           *int64 `json:"input_tokens"`
	OutputTokens          *int64 `json:"output_tokens"`
	CachedTokens          *int64 `json:"cached_tokens"`
	ReportedUsageAttempts int    `json:"reported_usage_attempts"`
}
type TrafficReason struct {
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
}
type TrafficProject struct {
	Project     string         `json:"project"`
	ProjectName string         `json:"project_name"`
	Requests    int            `json:"requests"`
	Attempts    int            `json:"attempts"`
	Recovered   int            `json:"recovered"`
	Accounts    map[string]int `json:"accounts"`
}
type TrafficDimension struct {
	Caller    string         `json:"caller,omitempty"`
	Session   string         `json:"session,omitempty"`
	Requests  int            `json:"requests"`
	Attempts  int            `json:"attempts"`
	Recovered int            `json:"recovered"`
	Accounts  map[string]int `json:"accounts"`
}

type TrafficFacetProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type TrafficFacets struct {
	Projects []TrafficFacetProject `json:"projects"`
	Callers  []string              `json:"callers"`
	Accounts []string              `json:"accounts"`
	Models   []string              `json:"models"`
}
type TrafficCoverage struct {
	ActivitySummary
	UnlinkedAttempts int `json:"unlinked_attempts"`
	RequestLimit     int `json:"request_limit"`
	AttemptLimit     int `json:"attempt_limit"`
}

type TrafficPage struct {
	AggregationLimited bool               `json:"aggregation_limited"`
	FacetsLimited      bool               `json:"facets_limited"`
	InstanceID         string             `json:"instance_id"`
	Revision           uint64             `json:"view_revision"`
	Coverage           TrafficCoverage    `json:"coverage"`
	Summary            TrafficSummary     `json:"summary"`
	Records            []TrafficRecord    `json:"records"`
	Record             *TrafficRecord     `json:"record,omitempty"`
	Page               int                `json:"page"`
	Limit              int                `json:"limit"`
	Total              int                `json:"total"`
	Accounts           []TrafficAccount   `json:"accounts"`
	Reasons            []TrafficReason    `json:"reasons"`
	Projects           []TrafficProject   `json:"projects"`
	Callers            []TrafficDimension `json:"callers"`
	Sessions           []TrafficDimension `json:"sessions"`
	Facets             TrafficFacets      `json:"facets"`
}

func mustTrafficJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func trafficHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
func markTrafficUnknown(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "UPDATE traffic_requests SET status='unknown',data=json_set(data,'$.status','unknown') WHERE status='running'")
	return err
}
func trafficText(s string) bool { return len(s) <= 256 && !strings.ContainsFunc(s, unicode.IsControl) }
func trafficReason(s string) string {
	switch s {
	case "session_affinity", "parent_affinity", "project_preference", "quota_load", "failover", "pinned", "host_selected", "scheduler_busy", "account_unavailable", "available_pool":
		return s
	}
	return "unknown"
}
func validTraffic(t *events.Traffic) bool {
	if t == nil {
		return true
	}
	if t.Phase == "" || t.Phase == "all" || !historyChoice(t.Phase, "started", "selected", "completed", "decision", "usage") {
		return false
	}
	for _, s := range []string{t.SelectionID, t.ExecutionID, t.TraceID, t.Caller, t.Session, t.ParentSession, t.Project, t.Model, t.AccountID, t.Provider, t.Reason, t.Status, t.RequestedAt, t.StartedAt, t.CompletedAt} {
		if !trafficText(s) {
			return false
		}
	}
	for _, n := range []*int64{t.LatencyNS, t.TTFTNS, t.InputTokens, t.OutputTokens, t.CachedTokens} {
		if n != nil && (*n < 0 || *n > math.MaxInt64/2) {
			return false
		}
	}
	return t.HTTPStatus == nil || (*t.HTTPStatus >= 100 && *t.HTTPStatus <= 599)
}

// All identity digests are salted per database. No raw caller/session material
// enters traffic tables; absent identities remain absent rather than invented.
func anonymizeTraffic(ctx context.Context, tx *sql.Tx, t events.Traffic) (events.Traffic, error) {
	var salt string
	if err := tx.QueryRowContext(ctx, "SELECT salt FROM traffic_meta WHERE singleton=1").Scan(&salt); err != nil {
		return t, err
	}
	caller := t.Caller
	if caller != "" {
		t.Caller = "caller-" + trafficHash(salt, "caller", caller)
	}
	if t.Session != "" && caller != "" {
		t.Session = "session-" + trafficHash(salt, "session", caller, t.Session)
	} else {
		t.Session = ""
	}
	if t.ParentSession != "" && caller != "" {
		t.ParentSession = "session-" + trafficHash(salt, "session", caller, t.ParentSession)
	} else {
		t.ParentSession = ""
	}
	t.Model = SanitizeWakeText(t.Model)
	evidence := map[string]any{}
	for _, key := range []string{"priority", "inflight", "max_inflight", "candidate_count", "quota_fresh", "snapshot_fresh", "project_preferred"} {
		switch v := t.Evidence[key].(type) {
		case bool:
			evidence[key] = v
		case float64:
			if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1000000 {
				evidence[key] = v
			}
		}
	}
	t.Evidence = evidence
	return t, nil
}

func loadTrafficRequest(ctx context.Context, tx *sql.Tx, id string) (TrafficRecord, error) {
	var raw string
	var r TrafficRecord
	err := tx.QueryRowContext(ctx, "SELECT data FROM traffic_requests WHERE id=?", id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &r)
	}
	return r, err
}
func saveTrafficRequest(ctx context.Context, tx *sql.Tx, instance, trace string, r TrafficRecord) error {
	r.Attempts = nil
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_requests(id,bridge_instance,request_id,trace_id,project,caller,session,model,account_id,status,created_at,updated_at,data) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET trace_id=CASE WHEN excluded.trace_id!='' THEN excluded.trace_id ELSE traffic_requests.trace_id END,project=excluded.project,caller=excluded.caller,session=excluded.session,model=excluded.model,account_id=excluded.account_id,status=excluded.status,updated_at=excluded.updated_at,data=excluded.data`, r.ID, instance, r.RequestID, trace, r.Project, r.Caller, r.Session, r.Model, r.AccountID, r.Status, r.CreatedAt, r.UpdatedAt, string(raw))
	return err
}
func mergeTrafficMetadata(r *TrafficRecord, t events.Traffic) {
	if t.Project != "" {
		r.Project = t.Project
	}
	if t.Caller != "" {
		r.Caller = t.Caller
	}
	if t.Session != "" {
		r.Session = t.Session
	}
	if t.ParentSession != "" {
		r.ParentSession = t.ParentSession
	}
	if t.Model != "" {
		r.Model = t.Model
	}
}
func nsMilliseconds(n *int64) *float64 {
	if n == nil || *n <= 0 {
		return nil
	}
	v := float64(*n) / 1e6
	return &v
}

func applyTraffic(ctx context.Context, tx *sql.Tx, instance string, e events.Event, cursor *EventCursor) error {
	if e.Traffic == nil {
		return nil
	}
	t, err := anonymizeTraffic(ctx, tx, *e.Traffic)
	if err != nil {
		return err
	}
	stamp := queryStamp(e.At)
	requestID := ""
	if t.Phase == "started" || t.Phase == "selected" || t.Phase == "completed" {
		requestID = trafficHash(instance, "request", e.RequestID)
		r, err := loadTrafficRequest(ctx, tx, requestID)
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM traffic_requests").Scan(&count); err != nil {
				return err
			}
			if count >= 50000 {
				cursor.Status, cursor.Reason = "gap", "traffic_capacity_exceeded"
				return nil
			}
			r = TrafficRecord{ID: requestID, RequestID: e.RequestID, Status: "running", CreatedAt: stamp, Reason: "unknown"}
		} else if err != nil {
			return err
		}
		mergeTrafficMetadata(&r, t)
		if stamp < r.UpdatedAt {
			stamp = r.UpdatedAt
		}
		r.UpdatedAt = stamp
		if t.Phase == "completed" {
			switch t.Status {
			case "succeeded":
				r.Status = "completed"
			case "failed":
				r.Status = "failed"
			case "rejected":
				r.Status = "not_sent"
			case "canceled":
				r.Status = "cancelled"
			default:
				r.Status = "unknown"
			}
			r.LatencyMS = nsMilliseconds(t.LatencyNS)
			start, a := time.Parse(time.RFC3339Nano, t.StartedAt)
			end, b := time.Parse(time.RFC3339Nano, t.CompletedAt)
			if a == nil && b == nil && !end.Before(start) {
				ms := float64(end.Sub(start)) / 1e6
				r.LatencyMS = &ms
			}
		}
		if t.AccountID != "" {
			r.AccountID = t.AccountID
		}
		if err = saveTrafficRequest(ctx, tx, instance, t.TraceID, r); err != nil {
			return err
		}
		if t.SelectionID != "" {
			if _, err = tx.ExecContext(ctx, "UPDATE traffic_attempts SET request_id=? WHERE bridge_instance=? AND selection_id=? AND (request_id='' OR request_id=?)", requestID, instance, t.SelectionID, requestID); err != nil {
				return err
			}
		}
		if t.TraceID != "" {
			var n int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM traffic_requests WHERE bridge_instance=? AND trace_id=?", instance, t.TraceID).Scan(&n); err != nil {
				return err
			}
			if n == 1 {
				if _, err = tx.ExecContext(ctx, "UPDATE traffic_attempts SET request_id=? WHERE bridge_instance=? AND trace_id=? AND request_id='' AND selection_id=''", requestID, instance, t.TraceID); err != nil {
					return err
				}
			} else if n > 1 {
				if _, err = tx.ExecContext(ctx, "UPDATE traffic_attempts SET request_id='' WHERE bridge_instance=? AND trace_id=? AND selection_id=''", instance, t.TraceID); err != nil {
					return err
				}
			}
		}
	}
	if t.SelectionID != "" || t.Phase == "usage" {
		key := "selection:" + t.SelectionID
		if t.Phase == "usage" {
			if t.ExecutionID == "" {
				return nil
			}
			key = "usage:" + t.ExecutionID
		}
		id := trafficHash(instance, key)
		var raw, existingRequest string
		var a TrafficAttempt
		err = tx.QueryRowContext(ctx, "SELECT data,request_id FROM traffic_attempts WHERE id=?", id).Scan(&raw, &existingRequest)
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &a); err != nil {
				return err
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM traffic_attempts").Scan(&count); err != nil {
				return err
			}
			if count >= 100000 {
				cursor.Status, cursor.Reason = "gap", "traffic_capacity_exceeded"
				return nil
			}
			a = TrafficAttempt{ID: id, SelectionID: t.SelectionID, CreatedAt: stamp, Reason: "unknown", Status: "selected", Evidence: map[string]any{}}
		} else {
			return err
		}
		if t.Phase == "usage" && t.SelectionID != "" && !a.Executed {
			var selectionRaw, linked string
			lookup := tx.QueryRowContext(ctx, "SELECT data,request_id FROM traffic_attempts WHERE id=?", trafficHash(instance, "selection:"+t.SelectionID)).Scan(&selectionRaw, &linked)
			if lookup == nil {
				var decision TrafficAttempt
				if err = json.Unmarshal([]byte(selectionRaw), &decision); err != nil {
					return err
				}
				decision.ID = id
				decision.ExecutionID = t.ExecutionID
				a = decision
				if existingRequest == "" {
					existingRequest = linked
				}
			} else if !errors.Is(lookup, sql.ErrNoRows) {
				return lookup
			}
		}
		if requestID == "" {
			requestID = existingRequest
		}
		if requestID == "" && t.TraceID != "" && t.SelectionID == "" {
			var count int
			err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(min(id),'') FROM traffic_requests WHERE bridge_instance=? AND trace_id=?", instance, t.TraceID).Scan(&count, &requestID)
			if err != nil {
				return err
			}
			if count != 1 {
				requestID = ""
			}
		}
		if t.AccountID != "" {
			a.AccountID = t.AccountID
		}
		if t.Model != "" {
			a.Model = t.Model
		}
		if t.Provider != "" {
			a.Provider = t.Provider
		}
		if t.Project != "" {
			a.Project = t.Project
		}
		if t.Caller != "" {
			a.Caller = t.Caller
		}
		if t.Session != "" {
			a.Session = t.Session
		}
		if t.ParentSession != "" {
			a.ParentSession = t.ParentSession
		}
		if t.Phase == "decision" && a.Reason == "unknown" {
			a.Reason = trafficReason(t.Reason)
			a.Evidence = t.Evidence
			if t.Status == "not_sent" && !a.Executed {
				a.Status = "not_sent"
			}
		}
		if t.Phase == "usage" {
			if at, err := time.Parse(time.RFC3339Nano, t.RequestedAt); a.Reason == "unknown" && err == nil && !at.After(e.At) {
				a.CreatedAt = queryStamp(at)
			}
			a.Executed = true
			a.ExecutionID = t.ExecutionID
			a.Status = t.Status
			if a.Status == "" || a.Status == "all" || !historyChoice(a.Status, "completed", "failed", "unknown") {
				a.Status = "unknown"
			}
			a.HTTPStatus = t.HTTPStatus
			a.LatencyMS = nsMilliseconds(t.LatencyNS)
			a.TTFTMS = nsMilliseconds(t.TTFTNS)
			a.InputTokens = t.InputTokens
			a.OutputTokens = t.OutputTokens
			a.CachedTokens = t.CachedTokens
		}
		a.UpdatedAt = stamp
		encoded, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO traffic_attempts(id,bridge_instance,selection_id,execution_id,request_id,trace_id,account_id,reason,status,executed,created_at,updated_at,data) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET execution_id=excluded.execution_id,request_id=CASE WHEN excluded.request_id!='' THEN excluded.request_id ELSE traffic_attempts.request_id END,trace_id=CASE WHEN excluded.trace_id!='' THEN excluded.trace_id ELSE traffic_attempts.trace_id END,account_id=excluded.account_id,reason=excluded.reason,status=excluded.status,executed=excluded.executed,updated_at=excluded.updated_at,data=excluded.data`, a.ID, instance, t.SelectionID, a.ExecutionID, requestID, t.TraceID, a.AccountID, a.Reason, a.Status, a.Executed, a.CreatedAt, a.UpdatedAt, string(encoded))
		if err != nil {
			return err
		}
		if t.Phase == "decision" {
			_, err = tx.ExecContext(ctx, `UPDATE traffic_attempts SET reason=?,data=json_set(data,'$.reason',?,'$.evidence',json(?),'$.caller',?,'$.session',?,'$.parent_session',?,'$.project',?) WHERE bridge_instance=? AND selection_id=? AND executed=1 AND reason='unknown'`, a.Reason, a.Reason, mustTrafficJSON(a.Evidence), a.Caller, a.Session, a.ParentSession, a.Project, instance, t.SelectionID)
			if err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE view_state SET revision=revision+1 WHERE singleton=1")
	return err
}

func addTrafficToken(sum **int64, value *int64) {
	if value == nil {
		return
	}
	if *sum == nil {
		v := int64(0)
		*sum = &v
	}
	if **sum > math.MaxInt64-*value {
		**sum = math.MaxInt64
	} else {
		**sum += *value
	}
}
func trafficMatches(r TrafficRecord, f TrafficFilter) bool {
	if f.Project != "" && f.Project != "all" && r.Project != f.Project {
		return false
	}
	if f.Caller != "" && f.Caller != "all" && r.Caller != f.Caller {
		return false
	}
	if f.Account != "" && f.Account != "all" {
		found := r.AccountID == f.Account
		for _, a := range r.Attempts {
			if a.Executed && a.AccountID == f.Account {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if f.Model != "" && f.Model != "all" && r.Model != f.Model {
		return false
	}
	if f.Status != "" && f.Status != "all" && r.Status != f.Status {
		return false
	}
	if f.Session != "" && r.Session != f.Session {
		return false
	}
	if f.From != "" && r.CreatedAt < f.From {
		return false
	}
	if f.To != "" && r.CreatedAt >= f.To {
		return false
	}
	values := []string{r.RequestID, r.Project, r.ProjectName, r.Caller, r.Session, r.Model, r.AccountID, r.Reason}
	for _, a := range r.Attempts {
		if a.Executed {
			values = append(values, a.AccountID, a.Reason)
		}
	}
	return f.Search == "" || strings.Contains(strings.ToLower(strings.Join(values, " ")), strings.ToLower(f.Search))
}
func normalizeTrafficFilter(f TrafficFilter) (TrafficFilter, error) {
	if f.Kind == "" {
		f.Kind = "users"
	}
	if f.Page == 0 {
		f.Page = 1
	}
	if f.Limit == 0 {
		f.Limit = 10
	}
	if f.Kind == "all" || !historyChoice(f.Kind, "users", "plugin", "analysis") || f.Page < 1 || f.Limit < 1 || f.Limit > 100 || f.Page > 1000000 {
		return f, ErrTrafficFilter
	}
	for _, s := range []string{f.ID, f.Search, f.Project, f.Caller, f.Account, f.Model, f.Status, f.Session} {
		if !trafficText(s) {
			return f, ErrTrafficFilter
		}
	}
	for _, value := range []*string{&f.From, &f.To} {
		if *value != "" {
			at, err := time.Parse(time.RFC3339Nano, *value)
			if err != nil {
				return f, ErrTrafficFilter
			}
			*value = queryStamp(at)
		}
	}
	if f.From != "" && f.To != "" && f.From >= f.To {
		return f, ErrTrafficFilter
	}
	return f, nil
}

func (s *Store) Traffic(ctx context.Context, f TrafficFilter) (TrafficPage, error) {
	p := TrafficPage{Records: []TrafficRecord{}, Accounts: []TrafficAccount{}, Reasons: []TrafficReason{}, Projects: []TrafficProject{}, Facets: TrafficFacets{Projects: []TrafficFacetProject{}, Callers: []string{}, Accounts: []string{}, Models: []string{}}}
	f, err := normalizeTrafficFilter(f)
	if err != nil {
		return p, err
	}
	// A bounded history scan can be sizable. Keep concurrent browser reads from
	// multiplying its memory budget; waiting readers remain cancellable.
	select {
	case s.trafficReader <- struct{}{}:
	case <-ctx.Done():
		return p, ctx.Err()
	}
	defer func() { <-s.trafficReader }()
	p.Page, p.Limit = f.Page, f.Limit
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&p.Revision); err != nil {
		return p, err
	}
	p.Coverage.EventCursor, err = readCursor(tx.QueryRowContext(ctx, cursorSQL))
	if err != nil {
		return p, err
	}
	p.Coverage.BaselineComplete = false
	p.Coverage.RequestLimit, p.Coverage.AttemptLimit = 50000, 100000
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM traffic_attempts WHERE executed=1 AND request_id=''").Scan(&p.Coverage.UnlinkedAttempts); err != nil {
		return p, err
	}

	var records []TrafficRecord
	if f.Kind == "plugin" {
		records, err = pluginTraffic(ctx, tx)
	} else {
		records, err = readUserTraffic(ctx, tx)
	}
	if err != nil {
		return p, err
	}
	for _, r := range records {
		if f.ID != "" {
			if r.ID == f.ID || r.RequestID == f.ID {
				copy := r
				p.Record = &copy
				p.Records = []TrafficRecord{r}
				p.Total = 1
				break
			}
			continue
		}
		if trafficMatches(r, f) {
			p.Records = append(p.Records, r)
		}
	}
	if f.ID != "" {
		if p.Record == nil {
			return p, ErrNotFound
		}
		return p, tx.Commit()
	}
	all := p.Records
	p.Callers, p.Sessions = []TrafficDimension{}, []TrafficDimension{}
	callerRows, sessionRows := map[string]*TrafficDimension{}, map[string]*TrafficDimension{}
	p.Total = len(all)
	p.Summary = TrafficSummary{Requests: len(all)}
	accounts := map[string]*TrafficAccount{}
	reasons := map[string]int{}
	projects := map[string]*TrafficProject{}
	callers, models := map[string]bool{}, map[string]bool{}
	for _, r := range all {
		for _, grouping := range []struct {
			key     string
			rows    map[string]*TrafficDimension
			session bool
		}{{r.Caller, callerRows, false}, {r.Session, sessionRows, true}} {
			if grouping.key == "" {
				continue
			}
			d := grouping.rows[grouping.key]
			if d == nil {
				d = &TrafficDimension{Accounts: map[string]int{}}
				if grouping.session {
					d.Session = grouping.key
				} else {
					d.Caller = grouping.key
				}
				grouping.rows[grouping.key] = d
			}
			d.Requests++
			d.Attempts += r.AttemptCount
			if r.Recovered {
				d.Recovered++
			}
			for _, a := range r.Attempts {
				if a.Executed {
					d.Accounts[a.AccountID]++
				}
			}
		}
		p.Summary.Attempts += r.AttemptCount
		if r.Recovered {
			p.Summary.Recovered++
		}
		if r.Status == "unknown" {
			p.Summary.Unknown++
		}
		if r.Caller != "" {
			callers[r.Caller] = true
		}
		if r.Model != "" {
			models[r.Model] = true
		}
		project := projects[r.Project]
		if project == nil {
			project = &TrafficProject{Project: r.Project, ProjectName: r.ProjectName, Accounts: map[string]int{}}
			projects[r.Project] = project
		}
		project.Requests++
		project.Attempts += r.AttemptCount
		if r.Recovered {
			project.Recovered++
		}
		for _, a := range r.Attempts {
			if !a.Executed {
				continue
			}
			account := accounts[a.AccountID]
			if account == nil {
				account = &TrafficAccount{AccountID: a.AccountID}
				accounts[a.AccountID] = account
			}
			account.Attempts++
			addTrafficToken(&account.InputTokens, a.InputTokens)
			addTrafficToken(&account.OutputTokens, a.OutputTokens)
			addTrafficToken(&account.CachedTokens, a.CachedTokens)
			if a.InputTokens != nil || a.OutputTokens != nil {
				account.ReportedUsageAttempts++
			}
			reasons[a.Reason]++
			project.Accounts[a.AccountID]++
		}
	}
	for _, d := range callerRows {
		p.Callers = append(p.Callers, *d)
	}
	for _, d := range sessionRows {
		p.Sessions = append(p.Sessions, *d)
	}
	sort.Slice(p.Callers, func(i, j int) bool {
		if p.Callers[i].Attempts == p.Callers[j].Attempts {
			return p.Callers[i].Caller < p.Callers[j].Caller
		}
		return p.Callers[i].Attempts > p.Callers[j].Attempts
	})
	sort.Slice(p.Sessions, func(i, j int) bool {
		if p.Sessions[i].Attempts == p.Sessions[j].Attempts {
			return p.Sessions[i].Session < p.Sessions[j].Session
		}
		return p.Sessions[i].Attempts > p.Sessions[j].Attempts
	})
	for _, a := range accounts {
		p.Accounts = append(p.Accounts, *a)
		p.Facets.Accounts = append(p.Facets.Accounts, a.AccountID)
	}
	sort.Slice(p.Accounts, func(i, j int) bool {
		if p.Accounts[i].Attempts == p.Accounts[j].Attempts {
			return p.Accounts[i].AccountID < p.Accounts[j].AccountID
		}
		return p.Accounts[i].Attempts > p.Accounts[j].Attempts
	})
	for reason, n := range reasons {
		p.Reasons = append(p.Reasons, TrafficReason{Reason: reason, Attempts: n})
	}
	sort.Slice(p.Reasons, func(i, j int) bool {
		if p.Reasons[i].Attempts == p.Reasons[j].Attempts {
			return p.Reasons[i].Reason < p.Reasons[j].Reason
		}
		return p.Reasons[i].Attempts > p.Reasons[j].Attempts
	})
	for _, project := range projects {
		p.Projects = append(p.Projects, *project)
		if project.Project != "" {
			p.Facets.Projects = append(p.Facets.Projects, TrafficFacetProject{ID: project.Project, Name: project.ProjectName})
		}
	}
	sort.Slice(p.Projects, func(i, j int) bool {
		if p.Projects[i].Attempts == p.Projects[j].Attempts {
			return p.Projects[i].Project < p.Projects[j].Project
		}
		return p.Projects[i].Attempts > p.Projects[j].Attempts
	})
	for c := range callers {
		p.Facets.Callers = append(p.Facets.Callers, c)
	}
	for m := range models {
		p.Facets.Models = append(p.Facets.Models, m)
	}
	sort.Slice(p.Facets.Projects, func(i, j int) bool { return p.Facets.Projects[i].ID < p.Facets.Projects[j].ID })
	sort.Strings(p.Facets.Accounts)
	sort.Strings(p.Facets.Callers)
	sort.Strings(p.Facets.Models)
	last := (len(all) + f.Limit - 1) / f.Limit
	if last < 1 {
		last = 1
	}
	if p.Page > last {
		p.Page = last
	}
	start := (p.Page - 1) * f.Limit
	end := start + f.Limit
	if end > len(all) {
		end = len(all)
	}
	p.Records = all[start:end]
	for i := range p.Records {
		p.Records[i].Attempts = nil
	}
	boundTrafficPage(&p)
	return p, tx.Commit()
}

func readUserTraffic(ctx context.Context, tx *sql.Tx) ([]TrafficRecord, error) {
	rows, err := tx.QueryContext(ctx, "SELECT r.id,r.data,COALESCE(p.name,'') FROM traffic_requests r LEFT JOIN routing_projects p ON p.project=r.project ORDER BY r.created_at DESC,r.id DESC LIMIT 50000")
	if err != nil {
		return nil, err
	}
	records := []TrafficRecord{}
	indices := map[string]int{}
	for rows.Next() {
		var id, raw, name string
		if err = rows.Scan(&id, &raw, &name); err != nil {
			rows.Close()
			return nil, err
		}
		var r TrafficRecord
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return nil, err
		}
		r.ProjectName = name
		r.Attempts = []TrafficAttempt{}
		indices[id] = len(records)
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT request_id,data FROM traffic_attempts WHERE request_id!='' ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		i, ok := indices[id]
		if !ok {
			continue
		}
		var a TrafficAttempt
		if err = json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		r := &records[i]
		mergeTrafficMetadata(r, events.Traffic{Project: a.Project, Caller: a.Caller, Session: a.Session, ParentSession: a.ParentSession, Model: a.Model})
		if !a.Executed {
			if r.Reason == "unknown" {
				r.Reason = a.Reason
				r.Evidence = a.Evidence
			}
			if a.Status == "not_sent" && r.Status == "failed" {
				r.Status = "not_sent"
			}
			continue
		}
		r.Attempts = append(r.Attempts, a)
		r.AttemptCount++
		r.Attempted = true
		if r.AccountID == "" || r.Status != "completed" {
			r.AccountID = a.AccountID
		}
		if r.Status != "completed" || r.AccountID == a.AccountID {
			r.Reason = a.Reason
		}
		addTrafficToken(&r.InputTokens, a.InputTokens)
		addTrafficToken(&r.OutputTokens, a.OutputTokens)
		addTrafficToken(&r.CachedTokens, a.CachedTokens)
		if a.InputTokens != nil || a.OutputTokens != nil {
			r.ReportedUsageAttempts++
		}
		if a.TTFTMS != nil {
			r.TTFTMS = a.TTFTMS
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	projectNames := map[string]string{}
	nameRows, nameErr := tx.QueryContext(ctx, "SELECT project,name FROM routing_projects")
	if nameErr != nil {
		return nil, nameErr
	}
	for nameRows.Next() {
		var key, name string
		if err = nameRows.Scan(&key, &name); err != nil {
			nameRows.Close()
			return nil, err
		}
		projectNames[key] = name
	}
	nameErr = nameRows.Err()
	nameRows.Close()
	if nameErr != nil {
		return nil, nameErr
	}
	for i := range records {
		r := &records[i]
		r.ProjectName = projectNames[r.Project]
		if r.AttemptCount == 0 {
			r.AccountID = ""
		}
		if r.Status != "completed" {
			continue
		}
		failed := map[string]bool{}
		completed := false
		for _, a := range r.Attempts {
			if a.Status == "failed" {
				failed[a.AccountID] = true
			}
			if a.Status == "completed" && a.AccountID == r.AccountID {
				completed = true
			}
		}
		if completed {
			for account := range failed {
				if account != r.AccountID {
					r.Recovered = true
				}
			}
		}
	}

	return records, nil
}
