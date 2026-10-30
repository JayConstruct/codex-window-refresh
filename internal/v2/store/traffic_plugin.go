package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
)

// Existing persisted operations are exposed with their actual evidence level.
// A worker starting is not proof that a remote server received a request.
func pluginTraffic(ctx context.Context, tx *sql.Tx) ([]TrafficRecord, error) {
	result := []TrafficRecord{}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.account_id,r.phase,r.started_at,COALESCE(group_concat(DISTINCT i.job_id),''),COALESCE(group_concat(DISTINCT p.trigger),''),COALESCE(max(i.error_code),''),COALESCE(max(j.updated_at),'')
 FROM query_runs r LEFT JOIN job_items i ON i.run_id=r.id LEFT JOIN query_plans p ON p.job_id=i.job_id LEFT JOIN jobs j ON j.id=i.job_id GROUP BY r.id ORDER BY r.started_at DESC LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r TrafficRecord
		var tasks, triggers, errorCode, updated string
		if err = rows.Scan(&r.ID, &r.AccountID, &r.Status, &r.CreatedAt, &tasks, &triggers, &errorCode, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		r.RequestID = r.ID
		r.ID = "query-" + r.ID
		r.Kind = "query"
		r.Stage = "quota_query"
		r.UpdatedAt = r.CreatedAt
		r.Evidence = map[string]any{"delivery": "unconfirmed", "source": "persisted_query_run"}
		r.Evidence["error_code"] = dashboardErrorCode(errorCode)
		if updated != "" {
			r.UpdatedAt = updated
		}
		switch {
		case r.Status == "finished" && errorCode == "":
			r.Status = "completed"
			r.Attempted = true
			r.AttemptCount = 1
			r.Evidence["delivery"] = "normalized_response_observed"
		case r.Status == "finished":
			r.Status = "failed"
			switch errorCode {
			case "quota_authentication_failed", "quota_upstream_forbidden", "quota_upstream_rate_limited", "quota_upstream_http_failed", "quota_body_failed", "quota_response_too_large", "quota_response_unrecognized":
				r.Attempted = true
				r.AttemptCount = 1
				r.Evidence["delivery"] = "response_observed"
			}
		case r.Status == "interrupted":
			r.Status = "unknown"
		}

		if tasks != "" {
			r.TaskIDs = splitNonempty(tasks)
			r.TaskID = r.TaskIDs[0]
		}
		r.Trigger = triggers
		if len(splitNonempty(triggers)) > 1 {
			r.Trigger = "shared_query"
		}
		result = append(result, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT w.id,w.account_id,w.reason,w.phase,w.created_at,w.updated_at,w.model,w.policy_revision,w.model_revision,w.query_job_id,COALESCE(r.detail,''),COALESCE(e.evidence,'') FROM wake_jobs w LEFT JOIN wake_responses r ON r.job_id=w.id LEFT JOIN traffic_operation_evidence e ON e.job_id=w.id ORDER BY w.created_at DESC LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r TrafficRecord
		var detail, query, snapshot string
		var policyRevision, modelRevision int64
		if err = rows.Scan(&r.ID, &r.AccountID, &r.Trigger, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.Model, &policyRevision, &modelRevision, &query, &detail, &snapshot); err != nil {
			rows.Close()
			return nil, err
		}
		r.TaskID = r.ID
		r.RequestID = r.ID
		r.ID = "wake-" + r.ID
		r.Kind = "wake"
		r.Evidence = map[string]any{"policy_revision": policyRevision, "model_revision": modelRevision, "query_job_id": query, "delivery": "unconfirmed"}
		if snapshot != "" {
			var evidence map[string]any
			if json.Unmarshal([]byte(snapshot), &evidence) == nil {
				for key, value := range evidence {
					r.Evidence[key] = value
				}
			}
		}

		if detail != "" {
			var response WakeResponse
			if json.Unmarshal([]byte(detail), &response) == nil {
				response = NormalizeWakeResponse(response)
				r.Response = &response
				r.Stage = response.Stage
				if response.HTTPStatus > 0 {
					status := response.HTTPStatus
					r.HTTPStatus = &status
					r.Evidence["delivery"] = "response_observed"
				}
				r.Attempted = response.Stage != "preflight" && response.Stage != ""
			}
		}
		if r.Status == "sending" || r.Status == "verifying" {
			r.Attempted = true
		}
		if r.Attempted {
			r.AttemptCount = 1
		}
		result = append(result, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,account_id,provider,model,phase,error_code,created_at,updated_at FROM prompt_generations ORDER BY created_at DESC LIMIT 10000")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r TrafficRecord
		var provider, code string
		if err = rows.Scan(&r.ID, &r.AccountID, &provider, &r.Model, &r.Status, &code, &r.CreatedAt, &r.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.TaskID = r.ID
		r.RequestID = r.ID
		r.ID = "generation-" + r.ID
		r.Kind = "generation"
		r.Trigger = "manual_generation"
		r.Stage = "candidate_generation"
		r.Evidence = map[string]any{"provider": provider, "error_code": NormalizeWakeResponse(WakeResponse{ErrorCode: code}).ErrorCode, "delivery": "unconfirmed"}
		if r.Status == "completed" {
			r.Attempted = true
			r.AttemptCount = 1
			r.Evidence["delivery"] = "model_completed"
		}
		result = append(result, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,account_id,phase,outcome,error_code,created_at,updated_at FROM reset_attempts ORDER BY created_at DESC LIMIT 10000")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r TrafficRecord
		var outcome, code string
		if err = rows.Scan(&r.ID, &r.AccountID, &r.Status, &outcome, &code, &r.CreatedAt, &r.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.TaskID = r.ID
		r.RequestID = r.ID
		r.ID = "reset-" + r.ID
		r.Kind = "reset"
		r.Trigger = "expiry_reset"
		r.Stage = "official_cli"
		r.Evidence = map[string]any{"outcome": outcome, "error_code": code, "delivery": "unconfirmed"}
		if r.Status == "sending" || r.Status == "unknown" || r.Status == "verifying" || outcome != "" {
			r.Attempted = true
			r.AttemptCount = 1
		}
		if outcome != "" {
			r.Evidence["delivery"] = "official_outcome_observed"
		}
		result = append(result, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt == result[j].CreatedAt {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result, nil
}

func splitNonempty(s string) []string {
	result := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	return result
}
