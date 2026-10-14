package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestLegacyStateLoadsAndNewTaskRecordsSurviveReopen(t *testing.T) {
	store, dir := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema":1,"state":{"Revision":0,"Accounts":{},"Activity":{},"Requests":{}}}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state.History = []core.HistoryEntry{{ID: 1, Kind: "generation", Prompt: "prompt", StartedAt: now, Status: "completed", Sources: []core.ActionSource{{Trigger: "prompt_generation"}}}}
	state.GenerationJob = &core.PromptGenerationJob{Request: core.PromptGenerationRequest{RequestID: "request-123", Count: 5}, Phase: "completed", Candidates: []string{"one"}}
	if _, err = store.Commit(context.Background(), 0, state); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err := store.Load(context.Background())
	if err != nil || len(restored.History) != 1 || restored.GenerationJob.Candidates[0] != "one" {
		t.Fatal("new persistent fields lost on reopen")
	}
	before, _ := json.Marshal(restored)
	restored.History[0].Sources[0].Trigger = "mutated"
	restored.GenerationJob.Candidates[0] = "mutated"
	again, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(again)
	if string(before) != string(after) {
		t.Fatal("loaded state leaked mutable history or generated candidates")
	}
}
