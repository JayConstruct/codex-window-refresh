package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type generationSource struct {
	dashboardSource
	calls    atomic.Int32
	active   atomic.Int32
	maximum  atomic.Int32
	generate func(context.Context, store.Account, string, int) ([]string, error)
}

func (source *generationSource) GenerateCandidates(ctx context.Context, account store.Account, model string, count int) ([]string, error) {
	source.calls.Add(1)
	active := source.active.Add(1)
	defer source.active.Add(-1)
	for previous := source.maximum.Load(); active > previous; previous = source.maximum.Load() {
		if source.maximum.CompareAndSwap(previous, active) {
			break
		}
	}
	return source.generate(ctx, account, model, count)
}

func generationService(t *testing.T, source *generationSource) (*Service, *store.Store) {
	t.Helper()
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	if err = storage.SyncInventory(context.Background(), []store.Account{{ID: "generation-account", AuthIndex: "index", Binding: "binding", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, source, Options{QueryConcurrency: 2, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return service, storage
}

const generationBody = `{"account_id":"generation-account","model":"test-model","count":2}`

func generationAccepted(t *testing.T, service *Service, key string) store.Generation {
	t.Helper()
	response := request(service, "POST", "/api/v2/prompt-generations", generationBody, key)
	var generation store.Generation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &generation) != nil || generation.ID == "" || generation.InstanceID == "" {
		t.Fatal(response.Code, response.Body.String())
	}
	return generation
}

func awaitGeneration(t *testing.T, storage *store.Store, id, phase string) store.Generation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		generation, err := storage.Generation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if generation.Phase == phase {
			return generation
		}
		time.Sleep(5 * time.Millisecond)
	}
	generation, _ := storage.Generation(context.Background(), id)
	t.Fatalf("wanted %s got %+v", phase, generation)
	return generation
}

func TestGenerationAPIValidationAndManualOnlyWorker(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	source := &generationSource{generate: func(ctx context.Context, account store.Account, model string, count int) ([]string, error) {
		if account.Binding != "binding" || account.AuthIndex != "index" || model != "test-model" || count != 2 {
			return nil, errors.New("bad test input")
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return []string{" hello ", "hello"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	service, storage := generationService(t, source)
	for _, body := range []string{`{}`, generationBody + ` {}`, strings.Replace(generationBody, `"count":2`, `"count":21`, 1), strings.Replace(generationBody, `"model":"test-model"`, `"model":"bad model"`, 1), strings.Replace(generationBody, `"count":2`, `"count":2,"prompt":"secret prompt"`, 1)} {
		response := request(service, "POST", "/api/v2/prompt-generations", body, "invalid")
		if response.Code != 400 || strings.Contains(response.Body.String(), "secret prompt") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if response := request(service, "POST", "/api/v2/prompt-generations", generationBody, ""); response.Code != 400 {
		t.Fatal(response.Code, response.Body.String())
	}
	first := generationAccepted(t, service, "first")
	if source.calls.Load() != 0 {
		t.Fatal("HTTP handler executed model")
	}
	second := generationAccepted(t, service, "second")
	if response := request(service, "POST", "/api/v2/prompt-generations", strings.Replace(generationBody, `"count":2`, `"count":1`, 1), "first"); response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if duplicate := generationAccepted(t, service, "first"); duplicate.ID != first.ID || duplicate.Phase != "running" {
		t.Fatal(duplicate)
	}
	if source.calls.Load() != 1 {
		t.Fatal("concurrent model requests")
	}
	close(release)
	finished := awaitGeneration(t, storage, first.ID, "completed")
	awaitGeneration(t, storage, second.ID, "completed")
	if len(finished.Candidates) != 1 || finished.Candidates[0] != "hello" || source.maximum.Load() != 1 || source.calls.Load() != 2 {
		t.Fatal(finished, source.maximum.Load(), source.calls.Load())
	}
	if source.queryCalls.Load() != 0 || source.accountCalls.Load() != 0 {
		t.Fatal("generation called quota or inventory source")
	}
	response := request(service, "GET", "/api/v2/prompt-generations?job_id="+first.ID, "", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "binding") || strings.Contains(response.Body.String(), "auth_index") {
		t.Fatal(response.Code, response.Body.String())
	}
	if response = request(service, "GET", "/api/v2/overview", "", ""); response.Code != 200 || !strings.Contains(response.Body.String(), `"prompt_generation":true`) || !strings.Contains(response.Body.String(), `"model_execution":false`) {
		t.Fatal(response.Body.String())
	}
}

func TestGenerationUnknownCancellationAndRestartNeverRedispatch(t *testing.T) {
	started := make(chan struct{})
	source := &generationSource{generate: func(ctx context.Context, _ store.Account, _ string, _ int) ([]string, error) {
		close(started)
		<-ctx.Done()
		return nil, errors.New("raw secret provider failure")
	}}
	service, storage := generationService(t, source)
	accepted := generationAccepted(t, service, "cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.generate(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	<-done
	generation := awaitGeneration(t, storage, accepted.ID, "unknown")
	if generation.ErrorCode != "generation_outcome_unknown" {
		t.Fatal(generation)
	}
	restarted, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	duplicate := generationAccepted(t, restarted, "cancelled")
	if duplicate.ID != accepted.ID || duplicate.Phase != "unknown" {
		t.Fatal(duplicate)
	}
	response := request(restarted, "GET", "/api/v2/prompt-generations?job_id="+accepted.ID, "", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "secret") || duplicate.InstanceID == accepted.InstanceID {
		t.Fatal(response.Body.String())
	}
	if _, err = storage.ClaimGeneration(context.Background()); !errors.Is(err, store.ErrNotFound) || source.calls.Load() != 1 {
		t.Fatal(err, source.calls.Load())
	}
}

func TestGenerationFailuresDoNotRetryOrPersistRawErrors(t *testing.T) {
	for _, test := range []struct{ code, phase, public string }{
		{"generation_delivery_unknown", "unknown", "generation_outcome_unknown"},
		{"generation_candidates_invalid", "failed", "generation_response_invalid"},
		{"account_disabled", "failed", "generation_account_unavailable"},
		{"generation_not_supported", "failed", "generation_request_rejected"},
		{"arbitrary raw credential token", "unknown", "generation_outcome_unknown"},
	} {
		t.Run(test.code, func(t *testing.T) {
			source := &generationSource{generate: func(context.Context, store.Account, string, int) ([]string, error) { return nil, errors.New(test.code) }}
			service, storage := generationService(t, source)
			accepted := generationAccepted(t, service, "test")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); service.generate(ctx) }()
			generation := awaitGeneration(t, storage, accepted.ID, test.phase)
			cancel()
			<-done
			if generation.ErrorCode != test.public || source.calls.Load() != 1 {
				t.Fatal(generation, source.calls.Load())
			}
			if _, err := storage.ClaimGeneration(context.Background()); !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationNewRecoversInterruptedAndRejectsChangedAccount(t *testing.T) {
	source := &generationSource{generate: func(context.Context, store.Account, string, int) ([]string, error) {
		t.Error("unexpected dispatch")
		return nil, nil
	}}
	service, storage := generationService(t, source)
	first := generationAccepted(t, service, "interrupted")
	if _, err := storage.ClaimGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if generation := generationAccepted(t, restarted, "interrupted"); generation.Phase != "unknown" || generation.ID != first.ID {
		t.Fatal(generation)
	}
	second := generationAccepted(t, restarted, "replacement")
	if err = storage.SyncInventory(context.Background(), []store.Account{{ID: "generation-account", AuthIndex: "index", Binding: "changed", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimGeneration(context.Background())
	if err != nil || claim.ID != second.ID || claim.Phase != "failed" {
		t.Fatal(claim, err)
	}
	if source.calls.Load() != 0 {
		t.Fatal(source.calls.Load())
	}
}

type unavailableGenerationSource struct{ generationSource }

func (*unavailableGenerationSource) GenerationAvailable() bool { return false }

func TestGenerationUnsupportedSourceIsNotAdvertisedOrDispatched(t *testing.T) {
	storage, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := &unavailableGenerationSource{}
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	response := request(service, "POST", "/api/v2/prompt-generations", generationBody, "test")
	if response.Code != 503 || source.calls.Load() != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "GET", "/api/v2/overview", "", "")
	if !strings.Contains(response.Body.String(), `"prompt_generation":false`) {
		t.Fatal(response.Body.String())
	}
}
