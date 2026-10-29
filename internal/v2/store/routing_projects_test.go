package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/policy"
)

func projectEvent(sequence uint64, project, name string, at time.Time) events.Event {
	return events.Event{Sequence: sequence, Kind: "project_observed", RequestID: project, Project: project, ProjectName: name, At: at}
}

func TestObservedProjectsReplayDoesNotChangePreferencesOrActivity(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute)
	project := "dir-" + strings.Repeat("a", 64)
	settings, _ := s.SchedulingSettings(ctx)
	c := settings.Config
	c.Projects = []policy.ProjectPreference{{Project: project, Name: "商城", Accounts: []string{"account-a"}}}
	saved, err := s.SaveScheduling(ctx, settings.Revision, c)
	if err != nil {
		t.Fatal(err)
	}
	b := batch(0, projectEvent(1, project, "%E4%B8%AD%E6%96%87%20repo", at))
	for i := 0; i < 2; i++ {
		if _, err = s.ApplyEvents(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ObservedProjects(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "中文 repo" || list[0].FirstSeen != queryStamp(at) {
		t.Fatal(list, err)
	}
	current, _ := s.SchedulingSettings(ctx)
	if current.Revision != saved.Revision || current.Config.Projects[0].Name != "商城" {
		t.Fatal("observation changed preferences", current)
	}
	overview, err := s.Overview(ctx)
	if err != nil || overview.Activity.ActiveRequests != 0 || overview.Activity.RecentUsers != 0 {
		t.Fatal(overview, err)
	}
	if _, err = s.ApplyEvents(ctx, batch(1, projectEvent(2, project, "new-name", at.Add(time.Second)))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyEvents(ctx, batch(2, projectEvent(3, project, "old-name", at.Add(-time.Second)))); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ObservedProjects(ctx)
	if list[0].Name != "new-name" || list[0].LastSeen != queryStamp(at.Add(time.Second)) {
		t.Fatal(list)
	}
	snapshot, err := s.RoutingSnapshot(ctx, time.Now())
	if err != nil || snapshot.Config.Projects[0].Name != "" {
		t.Fatal("name leaked into routing snapshot", snapshot, err)
	}
}

func TestObservedProjectsAreBoundedAndValidateNamesWithoutBlockingRouting(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	var sequence uint64
	for start := 0; start < ObservedProjectLimit+8; start += events.BatchLimit {
		b := batch(sequence)
		for i := start; i < start+events.BatchLimit && i < ObservedProjectLimit+8; i++ {
			sequence++
			b.Events = append(b.Events, projectEvent(sequence, fmt.Sprintf("dir-%064x", i), "in%0Avalid", at.Add(time.Duration(i)*time.Second)))
		}
		b.Latest = sequence
		if _, err := s.ApplyEvents(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ObservedProjects(ctx)
	if err != nil || len(list) != ObservedProjectLimit || list[0].Project != fmt.Sprintf("dir-%064x", ObservedProjectLimit+7) {
		t.Fatal(len(list), err)
	}
	for _, item := range list {
		if item.Name != "" {
			t.Fatal("unsafe name retained", item)
		}
	}
	invalid := projectEvent(sequence+1, "not-a-directory-id", "repo", at)
	if _, err = s.ApplyEvents(ctx, batch(sequence, invalid)); err == nil {
		t.Fatal("invalid project accepted")
	}
}

func TestSchemaTenUpgradePreservesProjectPreferencesAndCreatesObservationStore(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	config := policy.DefaultScheduling()
	config.Projects = []policy.ProjectPreference{{Project: "legacy", Accounts: []string{"account-a"}, Directories: []string{"/client/repo"}}}
	before, err := s.SaveScheduling(ctx, 0, config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec("DROP TRIGGER routing_projects_insert; DROP TRIGGER routing_projects_update; DROP TABLE routing_projects; UPDATE schema_version SET version=10")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.SchedulingSettings(ctx)
	if err != nil || after.Revision != before.Revision || after.Config.Projects[0].Directories[0] != "/client/repo" {
		t.Fatal(after, err)
	}
	project := "dir-" + strings.Repeat("b", 64)
	if _, err = s.ApplyEvents(ctx, batch(0, projectEvent(1, project, "repo", time.Now().UTC()))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.ObservedProjects(ctx)
	if err != nil || len(list) != 1 || list[0].Project != project {
		t.Fatal(list, err)
	}
}
