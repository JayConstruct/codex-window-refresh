package store

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/policy"
)

const ObservedProjectLimit = 1024

type ObservedProject struct {
	Project   string `json:"project"`
	Name      string `json:"name"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

func applyProject(ctx context.Context, tx *sql.Tx, event events.Event) error {
	name, err := url.PathUnescape(event.ProjectName)
	name = strings.TrimSpace(name)
	if err != nil || !policy.ProjectName(name) {
		name = ""
	}
	stamp := queryStamp(event.At)
	_, err = tx.ExecContext(ctx, `INSERT INTO routing_projects VALUES(?,?,?,?)
 ON CONFLICT(project) DO UPDATE SET
 name=CASE WHEN excluded.last_seen>=last_seen AND excluded.name!='' THEN excluded.name ELSE name END,
 first_seen=min(first_seen,excluded.first_seen),last_seen=max(last_seen,excluded.last_seen)
 WHERE excluded.last_seen>last_seen OR excluded.first_seen<first_seen`, event.Project, name, stamp, stamp)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM routing_projects WHERE project IN
 (SELECT project FROM routing_projects ORDER BY last_seen DESC,project LIMIT -1 OFFSET ?)`, ObservedProjectLimit)
	return err
}

func (s *Store) ObservedProjects(ctx context.Context) ([]ObservedProject, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT project,name,first_seen,last_seen FROM routing_projects ORDER BY last_seen DESC,project LIMIT ?", ObservedProjectLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ObservedProject{}
	for rows.Next() {
		var item ObservedProject
		if err = rows.Scan(&item.Project, &item.Name, &item.FirstSeen, &item.LastSeen); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
