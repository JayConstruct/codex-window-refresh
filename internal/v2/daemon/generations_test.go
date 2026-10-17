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
	for _, body := range []string{`{}`, generationBody + ` {}`, strings.Replace(generationBody, `"count":2`, `"count":21`, 1), strings.Replace(generationBody, `"model":"test-model"`, `"model":"bad model"`, 1), strings.Replace(generationBody, `"count":2`, `"count":2,"prompt":"secret prompt\u0000"`, 1)} {
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

type catalogueGenerationSource struct {
	generationSource
	directory      func(context.Context) ([]store.Account, error)
	resolve        func(context.Context, string) (store.Account, error)
	providers      []string
	unavailable    atomic.Bool
	directoryCalls atomic.Int32
	resolveCalls   atomic.Int32
}

func (source *catalogueGenerationSource) GenerationAccounts(ctx context.Context) ([]store.Account, error) {
	source.directoryCalls.Add(1)
	return source.directory(ctx)
}

func (source *catalogueGenerationSource) ResolveGenerationAccount(ctx context.Context, id string) (store.Account, error) {
	source.resolveCalls.Add(1)
	return source.resolve(ctx, id)
}

func (source *catalogueGenerationSource) GenerationProviders() []string { return source.providers }
func (source *catalogueGenerationSource) GenerationAvailable() bool {
	return !source.unavailable.Load()
}

func catalogueService(t *testing.T, source *catalogueGenerationSource) (*Service, *store.Store) {
	t.Helper()
	_, storage := generationService(t, &source.generationSource)
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return service, storage
}

func TestGenerationCatalogueIsBoundedReadOnlyAndRedacted(t *testing.T) {
	source := &catalogueGenerationSource{providers: []string{"codex", "antigravity", "codex", "not-supported"}}
	source.directory = func(ctx context.Context) ([]store.Account, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("directory read has no bounded deadline")
		}
		return []store.Account{
			{ID: "codex-account", AuthIndex: "secret-index", Binding: "secret-binding", Status: "available", Quota: json.RawMessage(`{"secret":"quota"}`)},
			{ID: "antigravity-account", Provider: "antigravity", AuthIndex: "secret-antig-index", Binding: "secret-antig-binding", Status: "disabled"},
			{ID: "missing-binding", Provider: "antigravity", Status: "binding_unavailable"},
		}, nil
	}
	service, storage := catalogueService(t, source)
	before, err := storage.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/generation-accounts", "", "")
	var directory struct {
		InstanceID string              `json:"instance_id"`
		Accounts   []map[string]string `json:"accounts"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &directory) != nil || directory.InstanceID != service.instance || len(directory.Accounts) != 3 || directory.Accounts[0]["provider"] != "codex" || directory.Accounts[1]["provider"] != "antigravity" || directory.Accounts[2]["status"] != "binding_unavailable" {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, entry := range directory.Accounts {
		if len(entry) != 3 {
			t.Fatal("unexpected directory fields", entry)
		}
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "auth_index") || strings.Contains(response.Body.String(), "quota") {
		t.Fatal(response.Body.String())
	}
	after, err := storage.Overview(context.Background())
	if err != nil || before.Revision != after.Revision || after.Dashboard.Total != 1 || source.calls.Load() != 0 || source.queryCalls.Load() != 0 || source.resolveCalls.Load() != 0 || source.accountCalls.Load() != 0 {
		t.Fatal(before, after, err)
	}
	response = request(service, "GET", "/api/v2/overview", "", "")
	if !strings.Contains(response.Body.String(), `"generation_accounts":true`) || !strings.Contains(response.Body.String(), `"prompt_generation_providers":["codex","antigravity"]`) {
		t.Fatal(response.Body.String())
	}
	source.directory = func(context.Context) ([]store.Account, error) { return nil, nil }
	response = request(service, "GET", "/api/v2/generation-accounts", "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"accounts":[]`) {
		t.Fatal(response.Body.String())
	}
	for _, directory := range []func(context.Context) ([]store.Account, error){
		func(context.Context) ([]store.Account, error) { return nil, errors.New("secret raw callback error") },
		func(context.Context) ([]store.Account, error) { return make([]store.Account, 10001), nil },
		func(context.Context) ([]store.Account, error) {
			return []store.Account{{ID: "same"}, {ID: "same"}}, nil
		},
		func(context.Context) ([]store.Account, error) { return []store.Account{{ID: "invalid\nid"}}, nil },
		func(context.Context) ([]store.Account, error) {
			return []store.Account{{ID: "valid", Provider: "unsupported"}}, nil
		},
	} {
		source.directory = directory
		response = request(service, "GET", "/api/v2/generation-accounts", "", "")
		if response.Code != 503 || strings.Contains(response.Body.String(), "secret") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestGenerationResolvedProviderReplaySurvivesUnavailableHost(t *testing.T) {
	account := store.Account{ID: "antigravity-account", Provider: "antigravity", AuthIndex: "antig-index", Binding: "antig-binding", Status: "available"}
	source := &catalogueGenerationSource{providers: []string{"codex", "antigravity"}}
	source.resolve = func(ctx context.Context, id string) (store.Account, error) {
		if id != account.ID {
			t.Error(id)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("resolve has no deadline")
		}
		return account, nil
	}
	service, storage := catalogueService(t, source)
	body := strings.Replace(generationBody, "generation-account", account.ID, 1)
	for _, field := range []string{`"provider":"antigravity"`, `"auth_index":"index"`, `"binding":"binding"`} {
		response := request(service, "POST", "/api/v2/prompt-generations", strings.TrimSuffix(body, "}")+","+field+"}", "invalid")
		if response.Code != 400 || source.resolveCalls.Load() != 0 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	response := request(service, "POST", "/api/v2/prompt-generations", body, "antig")
	var first store.Generation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &first) != nil || source.resolveCalls.Load() != 1 || source.calls.Load() != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	persisted, err := storage.Generation(context.Background(), first.ID)
	if err != nil || persisted.Account.Provider != account.Provider || persisted.Account.Binding != account.Binding {
		t.Fatal(persisted, err)
	}
	source.unavailable.Store(true)
	source.resolve = func(context.Context, string) (store.Account, error) {
		return store.Account{}, errors.New("account_not_found")
	}
	response = request(service, "POST", "/api/v2/prompt-generations", body, "antig")
	var duplicate store.Generation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &duplicate) != nil || duplicate.ID != first.ID || source.resolveCalls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/prompt-generations", strings.Replace(body, `"count":2`, `"count":1`, 1), "antig")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "idempotency_conflict") || source.resolveCalls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(service, "POST", "/api/v2/prompt-generations", body, "new")
	if response.Code != 503 || source.resolveCalls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	source.unavailable.Store(false)
	for _, code := range []string{"account_not_found", "account_disabled", "account_identity_unavailable", "account_binding_changed", "raw secret callback error", "generation_not_supported"} {
		source.resolve = func(context.Context, string) (store.Account, error) { return store.Account{}, errors.New(code) }
		response = request(service, "POST", "/api/v2/prompt-generations", body, "new")
		want := 409
		if code == "generation_not_supported" {
			want = 503
		}
		if response.Code != want || strings.Contains(response.Body.String(), "secret") {
			t.Fatal(code, response.Code, response.Body.String())
		}
	}
	source.resolve = func(context.Context, string) (store.Account, error) {
		account.ID = "different-id"
		return account, nil
	}
	response = request(service, "POST", "/api/v2/prompt-generations", body, "new")
	if response.Code != 409 || source.calls.Load() != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestGenerationAntigravityReplacementFailsBeforeModelExecute(t *testing.T) {
	account := store.Account{ID: "antigravity-account", Provider: "antigravity", AuthIndex: "antig-index", Binding: "original-binding", Status: "available"}
	source := &catalogueGenerationSource{providers: []string{"antigravity"}}
	source.resolve = func(context.Context, string) (store.Account, error) { return account, nil }
	var models atomic.Int32
	source.generate = func(ctx context.Context, snapshot store.Account, _ string, _ int) ([]string, error) {
		current, err := source.ResolveGenerationAccount(ctx, snapshot.ID)
		if err != nil || current.Provider != snapshot.Provider || current.AuthIndex != snapshot.AuthIndex || current.Binding != snapshot.Binding {
			return nil, errors.New("account_binding_changed")
		}
		models.Add(1)
		return []string{"hello"}, nil
	}
	service, storage := catalogueService(t, source)
	body := strings.Replace(generationBody, "generation-account", account.ID, 1)
	response := request(service, "POST", "/api/v2/prompt-generations", body, "replacement")
	var first store.Generation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &first) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	account.Binding = "replacement-binding"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.generate(ctx) }()
	finished := awaitGeneration(t, storage, first.ID, "failed")
	cancel()
	<-done
	if finished.ErrorCode != "generation_account_unavailable" || finished.Account.Provider != "antigravity" || models.Load() != 0 || source.queryCalls.Load() != 0 || source.accountCalls.Load() != 0 {
		t.Fatal(finished, models.Load())
	}
}
