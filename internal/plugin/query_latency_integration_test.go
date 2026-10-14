package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	scheduler "codex-window-refresh/internal/runtime"
	"codex-window-refresh/internal/storage"
)

type queryLatencyCallback struct {
	size     int
	calls    map[string]int
	selected []string
}

func (callback *queryLatencyCallback) Call(_ context.Context, method string, request, response any) error {
	callback.calls[method]++
	var selected struct {
		Index string `json:"auth_index"`
	}
	requestJSON, _ := json.Marshal(request)
	if err := json.Unmarshal(requestJSON, &selected); err != nil {
		return err
	}
	var value any
	switch method {
	case "host.auth.list":
		entries := make([]map[string]any, 0, callback.size)
		for index := 0; index < callback.size; index++ {
			entries = append(entries, map[string]any{"id": fmt.Sprintf("account-%03d", index), "auth_index": fmt.Sprintf("index-%03d", index), "provider": "codex"})
		}
		value = map[string]any{"files": entries}
	case "host.auth.get_runtime":
		callback.selected = append(callback.selected, selected.Index)
		value = map[string]any{"auth": map[string]any{"id": strings.Replace(selected.Index, "index-", "account-", 1), "auth_index": selected.Index, "provider": "codex"}}
	case "host.auth.get":
		callback.selected = append(callback.selected, selected.Index)
		value = map[string]any{"auth_index": selected.Index, "json": map[string]any{"type": "codex", "account_id": "upstream-" + selected.Index, "access_token": "test-token"}}
	case "host.http.operation_open":
		value = map[string]any{"operation_id": "quota-operation"}
	case "host.http.do":
		var upstream struct {
			Headers map[string][]string `json:"headers"`
		}
		if err := json.Unmarshal(requestJSON, &upstream); err != nil {
			return err
		}
		if got := upstream.Headers["Chatgpt-Account-Id"]; len(got) != 1 || got[0] != "upstream-index-000" {
			return fmt.Errorf("wrong upstream account: %v", got)
		}
		value = map[string]any{"StatusCode": 200, "Body": []byte(`{"rate_limit":{"allowed":true,"limit_reached":false}}`)}
	default:
		return fmt.Errorf("unexpected callback: %s", method)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, response)
}

// Exercise the real credential validation and native reader, rather than a
// quota-only fake: the total RPC count must stay constant as the pool grows.
func TestSelectedQuotaNativeRuntimeUsesNineHostCallsRegardlessOfPoolSize(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			callback := &queryLatencyCallback{size: size, calls: map[string]int{}}
			host := cpa.NewHost(callback, &cpa.NativeReader{Callback: callback})
			t.Cleanup(func() { _ = host.Close() })
			store, err := storage.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			engine, err := scheduler.New(config.Defaults(), host, store, scheduler.WallClock{}, scheduler.CryptoRandom{})
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(engine.Snapshot().Accounts) != size {
				t.Fatal("inventory bootstrap incomplete")
			}
			callback.calls, callback.selected = map[string]int{}, nil
			status, jobID := engine.QueueQuotaQueryJob(core.AccountID("account-000"))
			if status != "queued" {
				t.Fatal(status)
			}
			if err := engine.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			var query *scheduler.ManualQueryStatus
			for _, job := range engine.JobStatuses() {
				if job.JobID == jobID {
					query = job.QuotaQuery
				}
			}
			if query == nil || query.Phase != "completed" || query.Completed != 1 || query.Failed != 0 {
				t.Fatalf("query failed: %+v", query)
			}
			total := 0
			for _, count := range callback.calls {
				total += count
			}
			if total != 9 || callback.calls["host.auth.list"] != 0 || callback.calls["host.http.do"] != 1 || callback.calls["host.auth.get_runtime"] != 3 || callback.calls["host.auth.get"] != 4 {
				t.Fatalf("selected quota scaled with pool size %d: total=%d calls=%v", size, total, callback.calls)
			}
			for _, index := range callback.selected {
				if index != "index-000" {
					t.Fatalf("queried another credential: %s", index)
				}
			}
			t.Logf("pool=%d: selected quota completed with %d host calls and one upstream request", size, total)
		})
	}
}
