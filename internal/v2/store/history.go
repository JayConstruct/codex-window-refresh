package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrInvalidTaskHistoryFilter = errors.New("invalid_task_history_filter")

type TaskHistoryFilter struct {
	Search    string
	Type      string
	Source    string
	Trigger   string
	Status    string
	From      string
	To        string
	TodayFrom string
	TodayTo   string
	Page      int
	Limit     int
}

type TaskHistoryRecord struct {
	JobID       string `json:"job_id"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	Trigger     string `json:"trigger"`
	Phase       string `json:"phase"`
	Status      string `json:"status"`
	AccountID   string `json:"account_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ScheduledAt string `json:"scheduled_at"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	Model       string `json:"model"`
	Total       int    `json:"total"`
	Completed   int    `json:"completed"`
	Failed      int    `json:"failed"`
	Waiting     int    `json:"waiting"`
	Querying    int    `json:"querying"`
	ErrorCode   string `json:"error_code"`
	HoldReason  string `json:"hold_reason"`
	automatic   bool
}

type TaskHistoryStatusCounts struct {
	All       int `json:"all"`
	Running   int `json:"running"`
	Queued    int `json:"queued"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Unknown   int `json:"unknown"`
	Skipped   int `json:"skipped"`
	Cancelled int `json:"cancelled"`
}

type TaskHistorySummary struct {
	Today          int `json:"today"`
	TodayAutomatic int `json:"today_automatic"`
	TodayManual    int `json:"today_manual"`
	Running        int `json:"running"`
	Queued         int `json:"queued"`
	Completed      int `json:"completed"`
	Failed         int `json:"failed"`
	Unknown        int `json:"unknown"`
}

type TaskHistoryPage struct {
	InstanceID   string                  `json:"instance_id"`
	Revision     uint64                  `json:"view_revision"`
	Records      []TaskHistoryRecord     `json:"records"`
	Page         int                     `json:"page"`
	Limit        int                     `json:"limit"`
	Total        int                     `json:"total"`
	StatusCounts TaskHistoryStatusCounts `json:"status_counts"`
	Summary      TaskHistorySummary      `json:"summary"`
}

const taskHistorySQL = `WITH tasks AS (
 SELECT j.id AS job_id,'query' AS type,
 CASE WHEN COALESCE(p.automatic,0)=1 OR COALESCE(NULLIF(p.trigger,''),CASE WHEN j.target='' THEN 'manual_batch' ELSE 'manual_single' END)
 NOT IN ('manual_single','manual_batch') THEN 'automatic' ELSE 'manual' END AS source,
 COALESCE(NULLIF(p.trigger,''),CASE WHEN j.target='' THEN 'manual_batch' ELSE 'manual_single' END) AS trigger,
 j.phase AS phase,
 CASE WHEN j.phase IN ('queued','blocked') THEN 'queued'
 WHEN j.phase='running' AND EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=j.id AND i.phase='queued')
 AND NOT EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=j.id AND i.phase='running') THEN 'queued'
 WHEN j.phase IN ('running','completed','failed','cancelled','unknown') THEN j.phase ELSE 'unknown' END AS status,
 j.target AS account_id,j.created_at,j.updated_at,'' AS scheduled_at,'' AS started_at,'' AS model,
 CASE WHEN j.phase IN ('completed','failed','cancelled') THEN j.updated_at ELSE '' END AS finished_at,
 COALESCE(NULLIF(j.error_code,''),(SELECT i.error_code FROM job_items i WHERE i.job_id=j.id AND i.phase='failed' ORDER BY i.account_id LIMIT 1),'') AS error_code,
 '' AS hold_reason,COALESCE(p.automatic,0) AS automatic
 FROM jobs j LEFT JOIN query_plans p ON p.job_id=j.id
 UNION ALL
 SELECT w.id,'wake',CASE WHEN w.reason='manual' THEN 'manual' ELSE 'automatic' END,w.reason,w.phase,
 CASE WHEN w.phase IN ('checking','prepared','sending','verifying') THEN 'running'
 WHEN w.phase IN ('queued','completed','failed','unknown','skipped','cancelled') THEN w.phase ELSE 'unknown' END,
 w.account_id,w.created_at,w.updated_at,w.scheduled_at,'',
 CASE WHEN w.phase IN ('sending','verifying','unknown','completed')
 OR (w.phase='failed' AND EXISTS(SELECT 1 FROM wake_responses response WHERE response.job_id=w.id)) THEN w.model ELSE '' END,
 '' AS finished_at,CASE WHEN w.phase='skipped' THEN '' ELSE w.error_code END,
 CASE WHEN w.phase='skipped' AND w.hold_reason='' AND w.error_code='window_already_active' THEN w.error_code ELSE w.hold_reason END,0 FROM wake_jobs w
 ) `

func historyChoice(value string, choices ...string) bool {
	if value == "" || value == "all" {
		return true
	}
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func historyWakeCode(code string) string {
	switch code {
	case "window_verified", "window_already_active", "wake_rule_disabled":
		return code
	default:
		return NormalizeWakeResponse(WakeResponse{ErrorCode: code}).ErrorCode
	}
}

func historyQueryCode(code string) string {
	if code == "automatic_query_disabled" {
		return code
	}
	return dashboardErrorCode(code)
}

func historySearchValues(ctx context.Context, tx *sql.Tx, search string) ([]any, error) {
	rows, err := tx.QueryContext(ctx, taskHistorySQL+`SELECT DISTINCT type,error_code,model FROM tasks
 UNION SELECT 'query',error_code,'' FROM job_items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models, queryCodes, wakeCodes := []string{}, []string{}, []string{}
	search = strings.ToLower(search)
	for rows.Next() {
		var kind, code, model string
		if err = rows.Scan(&kind, &code, &model); err != nil {
			return nil, err
		}
		if kind == "query" {
			if strings.Contains(strings.ToLower(historyQueryCode(code)), search) {
				queryCodes = append(queryCodes, code)
			}
		} else {
			if strings.Contains(strings.ToLower(historyWakeCode(code)), search) {
				wakeCodes = append(wakeCodes, code)
			}
			if strings.Contains(strings.ToLower(SanitizeWakeText(model)), search) {
				models = append(models, model)
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	values := make([]any, 0, 3)
	for _, matches := range [][]string{models, queryCodes, wakeCodes} {
		raw, marshalErr := json.Marshal(matches)
		if marshalErr != nil {
			return nil, marshalErr
		}
		values = append(values, string(raw))
	}
	return values, nil
}

func historyHoldReason(code string) string {
	if wakeAccountCode(code) {
		return code
	}
	switch code {
	case "", "inventory_unavailable", "wake_rule_disabled", "outside_allowed_hours", "capacity_satisfied", "quota_unknown", "weekly_reset_conflict", "active_window", "window_already_active", "quota_blocked", "weekly_reset_unconfirmed", "activity_unknown", "account_busy", "model_busy", "verification_pending", "excluded_hours", "model_missing", "configuration_changed":
		return code
	default:
		return "wake_outcome_unknown"
	}
}

func historyTime(raw string, dateAllowed, inclusiveDateEnd bool) (time.Time, error) {
	if len(raw) > 64 {
		return time.Time{}, ErrInvalidTaskHistoryFilter
	}
	var at time.Time
	var err error
	if dateAllowed && len(raw) == len("2006-01-02") {
		at, err = time.Parse("2006-01-02", raw)
		if inclusiveDateEnd {
			at = at.AddDate(0, 0, 1)
		}
	} else {
		at, err = time.Parse(time.RFC3339Nano, raw)
	}
	at = at.UTC()
	if err != nil || at.Year() < 1 || at.Year() > 9999 {
		return time.Time{}, ErrInvalidTaskHistoryFilter
	}
	return at, nil
}

func normalizeHistoryFilter(filter TaskHistoryFilter, at time.Time) (TaskHistoryFilter, error) {
	if filter.Page == 0 {
		filter.Page = 1
	}
	if filter.Limit == 0 {
		filter.Limit = 10
	}
	if filter.Page < 1 || filter.Limit < 1 || filter.Limit > 100 || int64(filter.Page)-1 > math.MaxInt64/int64(filter.Limit) ||
		len(filter.Search) > 256 || !utf8.ValidString(filter.Search) || strings.ContainsFunc(filter.Search, unicode.IsControl) ||
		!historyChoice(filter.Type, "query", "wake") || !historyChoice(filter.Source, "manual", "automatic") ||
		!historyChoice(filter.Status, "running", "queued", "completed", "failed", "unknown", "skipped", "cancelled") ||
		!historyChoice(filter.Trigger, "manual_single", "manual_batch", "periodic", "five_hour_expiry", "weekly_expiry", "initial_sync", "wake_preflight", "wake_verification", "manual", "five_hour_reset", "weekly_reset", "daily") {
		return filter, ErrInvalidTaskHistoryFilter
	}
	for _, value := range []*string{&filter.From, &filter.To} {
		if *value == "" {
			continue
		}
		parsed, err := historyTime(*value, true, value == &filter.To)
		if err != nil {
			return filter, err
		}
		*value = queryStamp(parsed)
	}
	if filter.From != "" && filter.To != "" && filter.From >= filter.To {
		return filter, ErrInvalidTaskHistoryFilter
	}
	if (filter.TodayFrom == "") != (filter.TodayTo == "") {
		return filter, ErrInvalidTaskHistoryFilter
	}
	if filter.TodayFrom == "" {
		start := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
		filter.TodayFrom, filter.TodayTo = queryStamp(start), queryStamp(start.AddDate(0, 0, 1))
	} else {
		start, err := historyTime(filter.TodayFrom, false, false)
		if err != nil {
			return filter, err
		}
		end, err := historyTime(filter.TodayTo, false, false)
		if err != nil || !start.Before(end) {
			return filter, ErrInvalidTaskHistoryFilter
		}
		filter.TodayFrom, filter.TodayTo = queryStamp(start), queryStamp(end)
	}
	return filter, nil
}

func historyWhere(filter TaskHistoryFilter, searchValues []any) (string, []any) {
	where := "1=1"
	args := []any{}
	for _, field := range []struct{ column, value string }{{"type", filter.Type}, {"source", filter.Source}, {"trigger", filter.Trigger}} {
		if field.value != "" && field.value != "all" {
			where += " AND " + field.column + "=?"
			args = append(args, field.value)
		}
	}
	if filter.From != "" {
		where += " AND created_at>=?"
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where += " AND created_at<?"
		args = append(args, filter.To)
	}
	if filter.Search != "" {
		where += ` AND (instr(lower(job_id),lower(?))>0 OR instr(lower(account_id),lower(?))>0
 OR model IN (SELECT value FROM json_each(?))
 OR (type='query' AND error_code IN (SELECT value FROM json_each(?)))
 OR (type='wake' AND error_code IN (SELECT value FROM json_each(?)))
 OR (type='query' AND EXISTS(SELECT 1 FROM job_items i WHERE i.job_id=tasks.job_id
 AND (instr(lower(i.account_id),lower(?))>0 OR i.error_code IN (SELECT value FROM json_each(?))))))`
		args = append(args, filter.Search, filter.Search)
		args = append(args, searchValues...)
		args = append(args, filter.Search, searchValues[1])
	}
	return where, args
}

func (counts TaskHistoryStatusCounts) count(status string) int {
	switch status {
	case "running":
		return counts.Running
	case "queued":
		return counts.Queued
	case "completed":
		return counts.Completed
	case "failed":
		return counts.Failed
	case "unknown":
		return counts.Unknown
	case "skipped":
		return counts.Skipped
	case "cancelled":
		return counts.Cancelled
	default:
		return counts.All
	}
}

func (st *Store) TaskHistory(ctx context.Context, filter TaskHistoryFilter) (TaskHistoryPage, error) {
	page := TaskHistoryPage{Records: []TaskHistoryRecord{}}
	nowAt := time.Now()
	filter, err := normalizeHistoryFilter(filter, nowAt)
	if err != nil {
		return page, err
	}
	page.Page, page.Limit = filter.Page, filter.Limit
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM view_state WHERE singleton=1").Scan(&page.Revision); err != nil {
		return page, err
	}
	var searchValues []any
	if filter.Search != "" {
		searchValues, err = historySearchValues(ctx, tx, filter.Search)
		if err != nil {
			return page, err
		}
	}
	where, args := historyWhere(filter, searchValues)
	statArgs := append([]any{filter.TodayFrom, filter.TodayTo}, args...)
	err = tx.QueryRowContext(ctx, taskHistorySQL+`SELECT count(*),
 COALESCE(sum(status='running'),0),COALESCE(sum(status='queued'),0),COALESCE(sum(status='completed'),0),
 COALESCE(sum(status='failed'),0),COALESCE(sum(status='unknown'),0),COALESCE(sum(status='skipped'),0),COALESCE(sum(status='cancelled'),0),
 COALESCE(sum(created_at>=?1 AND created_at<?2),0),
 COALESCE(sum(created_at>=?1 AND created_at<?2 AND source='automatic'),0),
 COALESCE(sum(created_at>=?1 AND created_at<?2 AND source='manual'),0)
 FROM tasks WHERE `+where, statArgs...).Scan(&page.StatusCounts.All, &page.StatusCounts.Running, &page.StatusCounts.Queued,
		&page.StatusCounts.Completed, &page.StatusCounts.Failed, &page.StatusCounts.Unknown, &page.StatusCounts.Skipped, &page.StatusCounts.Cancelled,
		&page.Summary.Today, &page.Summary.TodayAutomatic, &page.Summary.TodayManual)
	if err != nil {
		return page, err
	}
	page.Summary.Running, page.Summary.Queued, page.Summary.Completed = page.StatusCounts.Running, page.StatusCounts.Queued, page.StatusCounts.Completed
	page.Summary.Failed, page.Summary.Unknown = page.StatusCounts.Failed, page.StatusCounts.Unknown
	page.Total = page.StatusCounts.count(filter.Status)
	lastPage := 1
	if page.Total > 0 {
		lastPage = (page.Total-1)/page.Limit + 1
	}
	if page.Page > lastPage {
		page.Page = lastPage
	}
	if filter.Status != "" && filter.Status != "all" {
		where += " AND status=?"
		args = append(args, filter.Status)
	}
	args = append(args, page.Limit, int64(page.Page-1)*int64(page.Limit))
	rows, err := tx.QueryContext(ctx, taskHistorySQL+`SELECT job_id,type,source,trigger,phase,status,account_id,created_at,updated_at,
 scheduled_at,started_at,finished_at,model,error_code,hold_reason,automatic FROM tasks WHERE `+where+` ORDER BY created_at DESC,type,job_id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var record TaskHistoryRecord
		if err = rows.Scan(&record.JobID, &record.Type, &record.Source, &record.Trigger, &record.Phase, &record.Status, &record.AccountID,
			&record.CreatedAt, &record.UpdatedAt, &record.ScheduledAt, &record.StartedAt, &record.FinishedAt, &record.Model, &record.ErrorCode, &record.HoldReason, &record.automatic); err != nil {
			rows.Close()
			return page, err
		}
		page.Records = append(page.Records, record)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	settings, err := readQuerySettings(ctx, tx)
	if err != nil {
		return page, err
	}
	for index := range page.Records {
		record := &page.Records[index]
		if record.Type == "query" {
			record.ErrorCode = historyQueryCode(record.ErrorCode)
			err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(i.phase='completed'),0),COALESCE(sum(i.phase='failed'),0),
 COALESCE(sum(i.phase='queued'),0),COALESCE(sum(i.phase='running'),0),
 COALESCE(min(CASE WHEN i.phase='queued' THEN NULLIF(q.scheduled_at,'') END),min(NULLIF(q.scheduled_at,'')),''),
 COALESCE(min(NULLIF(r.started_at,'')),'')
 FROM job_items i LEFT JOIN query_schedule q ON q.job_id=i.job_id AND q.account_id=i.account_id
 LEFT JOIN query_runs r ON r.id=i.run_id WHERE i.job_id=?`, record.JobID).Scan(&record.Total, &record.Completed, &record.Failed,
				&record.Waiting, &record.Querying, &record.ScheduledAt, &record.StartedAt)
			if err != nil {
				return page, err
			}
			if record.automatic && record.Waiting > 0 && record.Querying == 0 && !settings.Config.Allowed(nowAt) {
				record.HoldReason = "excluded_hours"
			}
		} else {
			record.ErrorCode = historyWakeCode(record.ErrorCode)
			record.HoldReason = historyHoldReason(record.HoldReason)
			record.Model = SanitizeWakeText(record.Model)
			record.Total = 1
			switch record.Status {
			case "completed":
				record.Completed = 1
			case "failed":
				record.Failed = 1
			case "queued":
				record.Waiting = 1
			case "running":
				record.Querying = 1
			}
		}
	}
	return page, tx.Commit()
}
