package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/v2/quota"
)

const DashboardQuotaMaxAge = 5 * time.Minute
const DashboardRecentLimit = 20

type DashboardTasks struct {
	Active   int `json:"active"`
	Querying int `json:"querying"`
}

type JobSummary struct {
	ID          string `json:"id"`
	Target      string `json:"target"`
	Phase       string `json:"phase"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Completed   int    `json:"completed"`
	Failed      int    `json:"failed"`
	Total       int    `json:"total"`
	ErrorCode   string `json:"error_code,omitempty"`
	Trigger     string `json:"trigger"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	Waiting     int    `json:"waiting"`
	Querying    int    `json:"querying"`
	HoldReason  string `json:"hold_reason,omitempty"`
}

type JobSummaryPage struct {
	Revision uint64       `json:"view_revision"`
	Jobs     []JobSummary `json:"jobs"`
}

type DashboardEvent struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	At        string `json:"at,omitempty"`
	Phase     string `json:"phase"`
	JobID     string `json:"job_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	Severity  string `json:"severity"`
}

type Dashboard struct {
	Revision         uint64           `json:"view_revision"`
	AccountsRevision uint64           `json:"accounts_revision"`
	Total            int              `json:"total"`
	Available        int              `json:"available"`
	Disabled         int              `json:"disabled"`
	Unavailable      int              `json:"unavailable"`
	Capacity         int              `json:"capacity"`
	UnknownQuota     int              `json:"unknown_quota"`
	Errors           int              `json:"errors"`
	Tasks            DashboardTasks   `json:"tasks"`
	RecentJobs       []JobSummary     `json:"recent_jobs"`
	ActiveJobs       []JobSummary     `json:"active_jobs"`
	RecentEvents     []DashboardEvent `json:"recent_events"`
}

func freshQuotaField(snapshot quota.Snapshot, field string, at time.Time) bool {
	observed := snapshot.ObservedAt
	if stamp, exists := snapshot.Fields[field]; exists {
		observed = stamp.ObservedAt
	}
	return !observed.IsZero() && !observed.After(at) && at.Sub(observed) <= DashboardQuotaMaxAge
}

func quotaState(raw []byte, at time.Time) (bool, bool) {
	var snapshot quota.Snapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Permission == nil || snapshot.FiveHour == nil || snapshot.Weekly == nil {
		return false, false
	}
	for _, field := range []string{"permission", "five_hour", "weekly"} {
		if !freshQuotaField(snapshot, field, at) {
			return false, false
		}
	}
	if *snapshot.Permission != core.PermissionAllowed && *snapshot.Permission != core.PermissionBlocked {
		return false, false
	}
	for _, window := range []*core.WindowEvidence{snapshot.FiveHour, snapshot.Weekly} {
		switch window.State {
		case core.WindowActive, core.WindowInactive, core.WindowExpired:
		default:
			return false, false
		}
		if window.Remaining == nil || math.IsNaN(window.Remaining.Value) || math.IsInf(window.Remaining.Value, 0) || window.Remaining.Value < 0 {
			return false, false
		}
		if window.State == core.WindowActive && (window.ResetAt.IsZero() || !at.Before(window.ResetAt)) {
			return false, false
		}
	}
	capacity := *snapshot.Permission == core.PermissionAllowed && snapshot.FiveHour.State == core.WindowActive && snapshot.FiveHour.Remaining.Value > 0 && snapshot.Weekly.State != core.WindowExpired && snapshot.Weekly.Remaining.Value > 0
	return true, capacity
}

func recentJobs(ctx context.Context, tx *sql.Tx, limit int) ([]JobSummary, error) {
	return jobSummaries(ctx, tx, limit, false)
}

func dashboardErrorCode(code string) string {
	switch code {
	case "", "quota_query_timeout", "query_interrupted", "bridge_unavailable", "bridge_busy", "bridge_instance_changed", "bridge_transport_failed", "bridge_protocol_incompatible", "bridge_response_invalid", "host_callback_failed", "inventory_too_large", "account_identity_ambiguous", "account_identity_unavailable", "account_binding_changed", "account_not_found", "quota_credential_missing", "quota_proxy_invalid", "quota_transport_failed", "quota_authentication_failed", "quota_upstream_forbidden", "quota_upstream_rate_limited", "quota_upstream_http_failed", "quota_body_failed", "quota_response_too_large", "quota_response_unrecognized", "quota_clock_changed", "quota_query_failed", "queue_full", "storage_unavailable":
		return code
	}
	return "quota_query_failed"
}

func jobSummaries(ctx context.Context, tx *sql.Tx, limit int, active bool) ([]JobSummary, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("invalid_limit")
	}
	order := "ORDER BY j.updated_at DESC,j.id DESC"
	if active {
		order = "WHERE j.phase IN ('queued','running','blocked') ORDER BY j.created_at,j.id"
	}
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT j.id,j.target,j.phase,j.created_at,j.updated_at,
 (SELECT count(*) FROM job_items i WHERE i.job_id=j.id AND i.phase='completed'),
 (SELECT count(*) FROM job_items i WHERE i.job_id=j.id AND i.phase='failed'),
 (SELECT count(*) FROM job_items i WHERE i.job_id=j.id),
 COALESCE(NULLIF(j.error_code,''),(SELECT i.error_code FROM job_items i WHERE i.job_id=j.id AND i.phase='failed' ORDER BY i.account_id LIMIT 1),''),
 COALESCE(p.trigger,CASE WHEN j.target='' THEN 'manual_batch' ELSE 'manual_single' END),
 COALESCE((SELECT min(q.scheduled_at) FROM query_schedule q JOIN job_items i ON i.job_id=q.job_id AND i.account_id=q.account_id WHERE i.job_id=j.id AND i.phase='queued'),''),
 (SELECT count(*) FROM job_items i WHERE i.job_id=j.id AND i.phase='queued'),
 (SELECT count(*) FROM job_items i WHERE i.job_id=j.id AND i.phase='running'),COALESCE(p.automatic,0)
 FROM jobs j LEFT JOIN query_plans p ON p.job_id=j.id `+order+` LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []JobSummary{}
	for rows.Next() {
		var job JobSummary
		var automatic bool
		if err = rows.Scan(&job.ID, &job.Target, &job.Phase, &job.CreatedAt, &job.UpdatedAt, &job.Completed, &job.Failed, &job.Total, &job.ErrorCode, &job.Trigger, &job.ScheduledAt, &job.Waiting, &job.Querying, &automatic); err != nil {
			return nil, err
		}
		job.ErrorCode = dashboardErrorCode(job.ErrorCode)
		if automatic && job.Waiting > 0 && job.Querying == 0 && !settings.Config.Allowed(time.Now()) {
			job.HoldReason = "excluded_hours"
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) RecentJobs(ctx context.Context, limit int) ([]JobSummary, error) {
	page, err := s.RecentJobsPage(ctx, limit)
	return page.Jobs, err
}

func (s *Store) RecentJobsPage(ctx context.Context, limit int) (JobSummaryPage, error) {
	page := JobSummaryPage{Jobs: []JobSummary{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&page.Revision); err != nil {
		return page, err
	}
	page.Jobs, err = recentJobs(ctx, tx, limit)
	if err != nil {
		return page, err
	}
	return page, tx.Commit()
}

func (s *Store) Dashboard(ctx context.Context) (Dashboard, error) {
	view, err := s.Overview(ctx)
	return view.Dashboard, err
}

func readDashboard(ctx context.Context, tx *sql.Tx, revision, accountsRevision uint64, inventory InventoryStatus) (Dashboard, error) {
	dashboard := Dashboard{Revision: revision, AccountsRevision: accountsRevision, RecentJobs: []JobSummary{}, ActiveJobs: []JobSummary{}, RecentEvents: []DashboardEvent{}}
	rows, err := tx.QueryContext(ctx, "SELECT status,COALESCE(quota,'') FROM accounts WHERE status!='removed'")
	if err != nil {
		return dashboard, err
	}
	defer rows.Close()
	at := time.Now()
	for rows.Next() {
		var status, raw string
		if err = rows.Scan(&status, &raw); err != nil {
			return dashboard, err
		}
		dashboard.Total++
		switch status {
		case "available":
			dashboard.Available++
		case "disabled":
			dashboard.Disabled++
		default:
			dashboard.Unavailable++
		}
		known, capacity := quotaState([]byte(raw), at)
		if !known {
			dashboard.UnknownQuota++
		}
		if status == "available" && capacity {
			dashboard.Capacity++
		}
	}
	if err = rows.Err(); err != nil {
		return dashboard, err
	}
	rows.Close()
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE phase IN ('queued','running','blocked')").Scan(&dashboard.Tasks.Active); err != nil {
		return dashboard, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM query_runs WHERE phase='running'").Scan(&dashboard.Tasks.Querying); err != nil {
		return dashboard, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE phase IN ('failed','blocked')").Scan(&dashboard.Errors); err != nil {
		return dashboard, err
	}
	var wakeErrors int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM wake_jobs WHERE phase IN ('failed','unknown')").Scan(&wakeErrors); err != nil {
		return dashboard, err
	}
	dashboard.Errors += wakeErrors
	dashboard.Errors += dashboard.Unavailable
	dashboard.RecentJobs, err = recentJobs(ctx, tx, DashboardRecentLimit)
	if err != nil {
		return dashboard, err
	}
	dashboard.ActiveJobs, err = jobSummaries(ctx, tx, DashboardRecentLimit, true)
	if err != nil {
		return dashboard, err
	}
	for _, job := range dashboard.RecentJobs {
		severity, code := "info", "quota_query_"+job.Phase
		detail := "额度查询状态更新"
		switch job.Phase {
		case "queued":
			detail = "额度查询等待执行"
		case "running":
			detail = "额度查询执行中"
		case "completed":
			detail = "额度查询已完成"
		case "failed":
			detail = "额度查询失败"
		case "blocked":
			detail = "额度查询等待 CPA 连接"
		}
		if job.Phase == "failed" || job.Phase == "blocked" {
			severity = "error"
		}
		dashboard.RecentEvents = append(dashboard.RecentEvents, DashboardEvent{ID: "job:" + job.ID, Kind: "quota_query", At: job.UpdatedAt, Phase: job.Phase, JobID: job.ID, AccountID: job.Target, Detail: detail, Code: code, Severity: severity})
	}
	wakeRows, err := tx.QueryContext(ctx, "SELECT id,account_id,phase,updated_at FROM wake_jobs ORDER BY updated_at DESC,id DESC LIMIT ?", DashboardRecentLimit)
	if err != nil {
		return dashboard, err
	}
	for wakeRows.Next() {
		var id, accountID, phase, updated string
		if err = wakeRows.Scan(&id, &accountID, &phase, &updated); err != nil {
			wakeRows.Close()
			return dashboard, err
		}
		severity, detail := "info", "模型唤醒任务状态更新"
		if phase == "unknown" {
			severity, detail = "error", "模型唤醒结果未知，禁止自动重发"
		} else if phase == "failed" {
			severity, detail = "error", "模型唤醒失败"
		} else if phase == "verifying" {
			detail = "模型已返回，等待窗口证据核实"
		} else if phase == "completed" {
			detail = "模型唤醒已核实活动窗口"
		}
		dashboard.RecentEvents = append(dashboard.RecentEvents, DashboardEvent{ID: "wake:" + id, Kind: "model_wake", At: updated, Phase: phase, JobID: id, AccountID: accountID, Detail: detail, Code: "model_wake_" + phase, Severity: severity})
	}
	err = wakeRows.Err()
	wakeRows.Close()
	if err != nil {
		return dashboard, err
	}
	if inventory.Status == "ready" && inventory.LastSuccessAt != "" {
		dashboard.RecentEvents = append(dashboard.RecentEvents, DashboardEvent{ID: "inventory", Kind: "inventory", At: inventory.LastSuccessAt, Phase: "ready", Detail: "CPA 账号清单已同步", Code: "inventory_synced", Severity: "info"})
	} else if inventory.Status == "unavailable" {
		dashboard.Errors++
		dashboard.RecentEvents = append(dashboard.RecentEvents, DashboardEvent{ID: "inventory", Kind: "inventory", Phase: "unavailable", Detail: "CPA 账号清单同步失败", Code: "inventory_sync_failed", Severity: "error"})
	}
	sort.SliceStable(dashboard.RecentEvents, func(first, second int) bool {
		if dashboard.RecentEvents[first].At == "" || dashboard.RecentEvents[second].At == "" {
			return dashboard.RecentEvents[first].At == "" && dashboard.RecentEvents[second].At != ""
		}
		return dashboard.RecentEvents[first].At > dashboard.RecentEvents[second].At
	})
	if len(dashboard.RecentEvents) > DashboardRecentLimit {
		dashboard.RecentEvents = dashboard.RecentEvents[:DashboardRecentLimit]
	}
	return dashboard, nil
}
