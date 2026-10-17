package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func generationInventory(t *testing.T, st *Store, status, binding string) {
	t.Helper()
	if err := st.SyncInventory(context.Background(), []Account{{ID: "generation-account", AuthIndex: "generation-index", Binding: binding, Status: status}}); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationDurableIdentityAndUnknownRecovery(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	generationInventory(t, st, "available", "binding-one")
	request := GenerationRequest{AccountID: "generation-account", Model: "test-model", Count: 3}
	first, existing, err := st.CreateGeneration(ctx, "one", request)
	if err != nil || existing || first.Phase != "queued" {
		t.Fatal(first, existing, err)
	}
	second, existing, err := st.CreateGeneration(ctx, "one", request)
	if err != nil || !existing || second.ID != first.ID {
		t.Fatal(second, existing, err)
	}
	changed := request
	changed.Count++
	if _, _, err = st.CreateGeneration(ctx, "one", changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	queued, _, err := st.CreateGeneration(ctx, "two", request)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimGeneration(ctx)
	if err != nil || claimed.ID != first.ID || claimed.Phase != "running" || claimed.Account.Binding != "binding-one" {
		t.Fatal(claimed, err)
	}
	if _, err = st.ClaimGeneration(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.RecoverGenerations(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.Generation(ctx, first.ID)
	if err != nil || recovered.Phase != "unknown" || recovered.ErrorCode != "generation_interrupted" {
		t.Fatal(recovered, err)
	}
	duplicate, existing, err := st.CreateGeneration(ctx, "one", request)
	if err != nil || !existing || duplicate.ID != first.ID || duplicate.Phase != "unknown" {
		t.Fatal(duplicate, existing, err)
	}
	claimed, err = st.ClaimGeneration(ctx)
	if err != nil || claimed.ID != queued.ID {
		t.Fatal(claimed, err)
	}
	if err = st.FinishGeneration(ctx, claimed.ID, "completed", "", []string{" hello ", "hello", "world"}); err != nil {
		t.Fatal(err)
	}
	completed, err := st.Generation(ctx, claimed.ID)
	if err != nil || !reflect.DeepEqual(completed.Candidates, []string{"hello", "world"}) || completed.Phase != "completed" {
		t.Fatal(completed, err)
	}
	if err = st.FinishGeneration(ctx, claimed.ID, "completed", "", []string{"hello", "world"}); err != nil {
		t.Fatal(err)
	}
	if err = st.FinishGeneration(ctx, claimed.ID, "unknown", "generation_outcome_unknown", nil); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = st.ClaimGeneration(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestGenerationRejectsUnsafeAccountsAndStaleInventory(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	request := GenerationRequest{AccountID: "generation-account", Model: "model", Count: 1}
	if _, _, err := st.CreateGeneration(ctx, "missing", request); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	for _, status := range []string{"disabled", "removed", "binding_unavailable"} {
		generationInventory(t, st, status, "binding")
		if _, _, err := st.CreateGeneration(ctx, status, request); !errors.Is(err, ErrGenerationAccount) {
			t.Fatal(status, err)
		}
	}
	generationInventory(t, st, "available", "")
	if _, _, err := st.CreateGeneration(ctx, "empty-binding", request); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	generationInventory(t, st, "available", "one")
	if _, err := st.db.Exec("UPDATE inventory_state SET last_success_at=?", time.Now().Add(-91*time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateGeneration(ctx, "stale", request); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	generationInventory(t, st, "available", "one")
	first, _, err := st.CreateGeneration(ctx, "accepted", request)
	if err != nil {
		t.Fatal(err)
	}
	generationInventory(t, st, "available", "replacement")
	claim, err := st.ClaimGeneration(ctx)
	if err != nil || claim.ID != first.ID || claim.Phase != "failed" || claim.ErrorCode != "generation_account_unavailable" {
		t.Fatal(claim, err)
	}
	if err = st.SyncInventory(ctx, []Account{{ID: "generation-account", AuthIndex: "same", Binding: "one", Status: "available"}, {ID: "another", AuthIndex: "same", Binding: "two", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateGeneration(ctx, "ambiguous", request); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
}

func TestGenerationBoundedQueueAndConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	generationInventory(t, st, "available", "binding")
	request := GenerationRequest{AccountID: "generation-account", Model: "model", Count: 1}
	for index := 0; index < 64; index++ {
		if _, _, err := st.CreateGeneration(ctx, fmt.Sprint(index), request); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreateGeneration(ctx, "full", request); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, existing, err := st.CreateGeneration(ctx, "0", request); err != nil || !existing {
		t.Fatal(existing, err)
	}
	var group sync.WaitGroup
	var mutex sync.Mutex
	claims := 0
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := st.ClaimGeneration(ctx); err == nil {
				mutex.Lock()
				claims++
				mutex.Unlock()
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if claims != 1 {
		t.Fatal(claims)
	}
	if _, err := st.db.Exec("UPDATE prompt_generations SET phase='completed'"); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := 64; index < 1000; index++ {
		if _, err := tx.Exec("INSERT INTO prompt_generations(id,idempotency_key,request_hash,account_id,auth_index,binding,model,count,phase,candidates,created_at,updated_at) VALUES(?,?,?,'a','i','b','m',1,'completed','[]','2026','2026')", fmt.Sprint(index), fmt.Sprint(index), "hash"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateGeneration(ctx, "history-full", request); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestGenerationValidatesRequestsAndCandidates(t *testing.T) {
	request := GenerationRequest{AccountID: "account", Model: "model", Count: 1}
	for _, model := range []string{"", "has space", "has\nnewline", strings.Repeat("a", 201), string([]byte{255})} {
		request.Model = model
		if request.Validate() == nil {
			t.Fatal(model)
		}
	}
	for _, candidates := range [][]string{nil, {""}, {"one\ntwo"}, {"one\rtwo"}, {"one\u2028two"}, {"one\u2029two"}, {strings.Repeat("a", 513)}, {"bad\x00value"}, {string([]byte{255})}, {"one", "two"}} {
		if _, err := NormalizeGenerationCandidates(candidates, 1); err == nil {
			t.Fatal(candidates)
		}
	}
}

func TestGenerationAntigravitySnapshotIsIndependentAndDurable(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	request := GenerationRequest{AccountID: "antigravity-account", Model: "test-model", Count: 1}
	account := Account{ID: request.AccountID, Provider: "antigravity", AuthIndex: "antigravity-index", Binding: "antigravity-binding", Status: "available"}
	first, existing, err := st.CreateGenerationFor(ctx, "antigravity-key", request, account)
	if err != nil || existing || first.Account.Provider != account.Provider {
		t.Fatal(first, existing, err)
	}
	accounts, err := st.Accounts(ctx, "", 10)
	if err != nil || len(accounts) != 0 {
		t.Fatal("generation directory polluted quota inventory", accounts, err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	claim, err := st.ClaimGeneration(ctx)
	if err != nil || claim.ID != first.ID || claim.Phase != "running" || !reflect.DeepEqual(claim.Account, account) {
		t.Fatal(claim, err)
	}
	if err = st.FinishGeneration(ctx, claim.ID, "completed", "", []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	finished, err := st.Generation(ctx, first.ID)
	if err != nil || finished.Account.Provider != "antigravity" || finished.Account.Binding != account.Binding {
		t.Fatal(finished, err)
	}
	account.Provider = "codex"
	account.Binding = "replacement"
	duplicate, existing, err := st.CreateGenerationFor(ctx, "antigravity-key", request, account)
	if err != nil || !existing || duplicate.ID != first.ID || duplicate.Account.Provider != "antigravity" || duplicate.Account.Binding != "antigravity-binding" {
		t.Fatal(duplicate, existing, err)
	}
	duplicate, existing, err = st.LookupGeneration(ctx, "antigravity-key", request)
	if err != nil || !existing || duplicate.ID != first.ID || duplicate.Phase != "completed" {
		t.Fatal(duplicate, existing, err)
	}
}

func TestGenerationResolvedAccountValidationAndCodexFreshness(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	request := GenerationRequest{AccountID: "generation-account", Model: "model", Count: 1}
	base := Account{ID: request.AccountID, Provider: "antigravity", AuthIndex: "index", Binding: "binding", Status: "available"}
	for _, alter := range []func(*Account){
		func(account *Account) { account.ID = "another" },
		func(account *Account) { account.Provider = "unknown-provider" },
		func(account *Account) { account.Status = "disabled" },
		func(account *Account) { account.Status = "removed" },
		func(account *Account) { account.AuthIndex = "" },
		func(account *Account) { account.Binding = "" },
	} {
		account := base
		alter(&account)
		if _, _, err := st.CreateGenerationFor(ctx, "rejected", request, account); !errors.Is(err, ErrGenerationAccount) {
			t.Fatal(account, err)
		}
	}
	base.Provider = "codex"
	if _, _, err := st.CreateGenerationFor(ctx, "no-inventory", request, base); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	generationInventory(t, st, "available", "binding")
	base.AuthIndex = "generation-index"
	base.Binding = "replacement"
	if _, _, err := st.CreateGenerationFor(ctx, "replaced", request, base); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	base.Binding = "binding"
	base.Provider = ""
	first, _, err := st.CreateGenerationFor(ctx, "legacy-provider", request, base)
	if err != nil || first.Account.Provider != "codex" {
		t.Fatal(first, err)
	}
	if _, err = st.db.Exec("UPDATE prompt_generations SET provider='' WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.Generation(ctx, first.ID)
	if err != nil || legacy.Account.Provider != "codex" {
		t.Fatal(legacy, err)
	}
	if _, err = st.db.Exec("UPDATE inventory_state SET last_success_at=?", time.Now().Add(-91*time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateGenerationFor(ctx, "stale", request, base); !errors.Is(err, ErrGenerationAccount) {
		t.Fatal(err)
	}
	if claim, err := st.ClaimGeneration(ctx); err != nil || claim.Phase != "failed" {
		t.Fatal(claim, err)
	}
}

func TestLookupGenerationValidatesWithoutCreatingOrInventory(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	request := GenerationRequest{AccountID: "account", Model: "model", Count: 1}
	if _, existing, err := st.LookupGeneration(ctx, "absent", request); err != nil || existing {
		t.Fatal(existing, err)
	}
	for _, key := range []string{"", "invalid key", strings.Repeat("x", 129), "\x7f"} {
		if _, _, err := st.LookupGeneration(ctx, key, request); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(key, err)
		}
	}
	if _, _, err := st.LookupGeneration(ctx, "valid", GenerationRequest{}); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatal(err)
	}
	account := Account{ID: request.AccountID, Provider: "antigravity", AuthIndex: "index", Binding: "binding", Status: "available"}
	if _, _, err := st.CreateGenerationFor(ctx, "existing", request, account); err != nil {
		t.Fatal(err)
	}
	request.Model = "other-model"
	if _, existing, err := st.LookupGeneration(ctx, "existing", request); !existing || !errors.Is(err, ErrConflict) {
		t.Fatal(existing, err)
	}
	var total int
	if err := st.db.QueryRow("SELECT count(*) FROM prompt_generations").Scan(&total); err != nil || total != 1 {
		t.Fatal(total, err)
	}
}
