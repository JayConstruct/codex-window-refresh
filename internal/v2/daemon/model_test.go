package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

func modelPayload(revision uint64, config policy.ModelConfig) string {
	raw, _ := json.Marshal(map[string]any{"revision": revision, "config": config})
	return string(raw)
}

func TestModelSettingsAPIValidationConflictAndNoNetwork(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := &dashboardSource{}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/model-settings", "", "")
	var initial struct {
		store.ModelSettings
		InstanceID string `json:"instance_id"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &initial) != nil || initial.InstanceID == "" || initial.Revision != 0 || !reflect.DeepEqual(initial.Config, policy.DefaultModel()) {
		t.Fatal(response.Code, response.Body.String())
	}
	queriesBefore, err := storage.QuerySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	config := initial.Config
	config.DefaultModel = "fixture-model"
	config.PromptMode = "random"
	config.Phrases = []string{"  候选一  ", "候选二"}
	response = request(service, "POST", "/api/v2/model-settings", modelPayload(0, config), "")
	var saved store.ModelSettings
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Revision != 1 || saved.ViewRevision <= initial.ViewRevision || saved.Config.Phrases[0] != "候选一" {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/model-settings", modelPayload(0, config), "")
	if response.Code != 409 || response.Body.String() != "{\"error\":\"model_settings_conflict\"}\n" {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/model-settings", modelPayload(1, config), "")
	var unchanged store.ModelSettings
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &unchanged) != nil || unchanged.Revision != saved.Revision || unchanged.ViewRevision != saved.ViewRevision {
		t.Fatal(response.Code, response.Body.String())
	}
	valid := modelPayload(1, config)
	for _, body := range []string{
		`{}`, `{"revision":1}`, `{"revision":1,"config":null}`, `{"config":{}}`,
		`{"revision":1,"config":{}}`, `{"revision":-1,"config":{}}`, valid + ` {}`,
		strings.Replace(valid, `"config":`, `"private_token":"must-not-export","config":`, 1),
		strings.Replace(valid, `"default_model":`, `"api_key":"must-not-export","default_model":`, 1),
		strings.Replace(valid, `"default_model":"fixture-model"`, `"default_model":"bad model"`, 1),
		strings.Replace(valid, `"prompt_mode":"random"`, `"prompt_mode":"unknown"`, 1),
		strings.Repeat(" ", 128*1024) + valid,
		strings.Replace(valid, "fixture-model", string([]byte{0xff}), 1),
	} {
		response = request(service, "POST", "/api/v2/model-settings", body, "")
		if response.Code != 400 || response.Body.String() != "{\"error\":\"invalid_model_settings\"}\n" {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	response = request(service, "GET", "/api/v2/model-settings", "", "")
	var current store.ModelSettings
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &current) != nil || !reflect.DeepEqual(current, saved) {
		t.Fatal(response.Code, response.Body.String())
	}
	queriesAfter, err := storage.QuerySettings(context.Background())
	if err != nil || queriesAfter.Revision != queriesBefore.Revision || !reflect.DeepEqual(queriesBefore.Config, queriesAfter.Config) {
		t.Fatal(queriesAfter, err)
	}
	response = request(service, "GET", "/api/v2/overview", "", "")
	var overview struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &overview) != nil || overview.Capabilities["model_settings"] != true || overview.Capabilities["model_execution"] != false {
		t.Fatal(response.Code, response.Body.String())
	}
	if source.accountCalls.Load() != 0 || source.queryCalls.Load() != 0 {
		t.Fatal("model configuration dispatched host or quota work")
	}
	response = request(service, "DELETE", "/api/v2/model-settings", "", "")
	if response.Code != 404 {
		t.Fatal(response.Code)
	}
}

func TestModelSettingsAPIRedactsStorageFailures(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, &dashboardSource{}, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		response := request(service, method, "/api/v2/model-settings", modelPayload(0, policy.DefaultModel()), "")
		if response.Code != 503 || response.Body.String() != "{\"error\":\"storage_unavailable\"}\n" {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}
