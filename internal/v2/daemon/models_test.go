package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/executor"
	"codex-window-refresh/internal/v2/store"
)

type modelDirectorySource struct {
	dashboardSource
	reads int
	err   error
}

func (s *modelDirectorySource) AccountModels(_ context.Context, id string) (executor.AccountModelCatalog, error) {
	s.reads++
	return executor.AccountModelCatalog{AccountID: id, Provider: "codex", Models: []string{"local-model"}}, s.err
}

func TestAccountModelDirectoryAPIIsReadOnlyAndRejectsUnsafeQueries(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := &modelDirectorySource{}
	service, err := New(st, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	w := request(service, "GET", "/api/v2/overview", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"account_models":true`) || source.reads != 0 {
		t.Fatal(w.Code, w.Body.String(), source.reads)
	}
	w = request(service, "GET", "/api/v2/account-models?account_id=a", "", "")
	var result struct {
		executor.AccountModelCatalog
		Instance string `json:"instance_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Instance != service.instance || result.AccountID != "a" || len(result.Models) != 1 || source.reads != 1 || source.queryCalls.Load() != 0 || source.accountCalls.Load() != 0 {
		t.Fatal(w.Code, w.Body.String(), source.reads)
	}
	for _, query := range []string{"", "?account_id=", "?account_id=%09", "?account_id=%FF", "?account_id=a&account_id=b", "?account_id=a&provider=other"} {
		w = request(service, "GET", "/api/v2/account-models"+query, "", "")
		if w.Code != 400 || source.reads != 1 {
			t.Fatal(query, w.Code, w.Body.String(), source.reads)
		}
	}
	for _, test := range []struct {
		err  string
		code int
		body string
	}{
		{"model_catalog_unsupported", 501, "model_catalog_unsupported"},
		{"model_catalog_changed", 409, "model_catalog_changed"},
		{"model_catalog_account_invalid", 409, "model_catalog_account_invalid"},
		{"private-token-and-path", 503, "model_catalog_unavailable"},
	} {
		source.err = errors.New(test.err)
		w = request(service, "GET", "/api/v2/account-models?account_id=a", "", "")
		if w.Code != test.code || !strings.Contains(w.Body.String(), test.body) || strings.Contains(w.Body.String(), "private-token") {
			t.Fatal(test, w.Code, w.Body.String())
		}
	}
}
