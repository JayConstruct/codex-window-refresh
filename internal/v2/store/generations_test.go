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
