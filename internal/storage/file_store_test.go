//go:build linux

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func openTestStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

func dispatchState() core.CoreState {
	due := time.Date(2026, 10, 11, 2, 8, 0, 0, time.UTC)
	return core.CoreState{
		Accounts: map[core.AccountID]core.AccountState{
			"account-a": {ID: "account-a", Generation: "generation-1", Enabled: true,
				FiveHour: core.WindowEvidence{Remaining: &core.QuotaAmount{Value: 37, Unit: "percent"}},
				Task: &core.MergedTask{AccountID: "account-a", State: core.TaskDispatchIntent, OperationID: "operation-a", DispatchIntentAt: due,
					Intents: []core.Intent{{ID: "event-a", CycleID: "cycle-a", Reason: core.ReasonWeekly, RandomizedDueAt: due, Status: core.IntentPending}}, CoveredEventIDs: []core.EventID{"event-a"}}},
		},
		Reservations: []core.Reservation{{ID: "reservation-a", AccountID: "account-a", OperationID: "operation-a", Sent: true}},
	}
}

func TestCommitLoadReopenAndDeepCopy(t *testing.T) {
	store, dir := openTestStore(t)
	initial, err := store.Load(context.Background())
	if err != nil || initial.Revision != 0 || initial.Accounts == nil || initial.Activity == nil || initial.Requests == nil {
		t.Fatalf("initial state = %#v, %v", initial, err)
	}
	next := dispatchState()
	revision, err := store.Commit(context.Background(), 0, next)
	if err != nil || revision != 1 {
		t.Fatalf("commit = %d, %v", revision, err)
	}
	if next.Revision != 0 {
		t.Fatal("commit mutated caller's revision")
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := state.Accounts["account-a"]
	account.Task.Intents[0].RandomizedDueAt = time.Time{}
	account.Task.CoveredEventIDs[0] = "changed"
	account.FiveHour.Remaining.Value = 0
	state.Reservations[0].Sent = false
	delete(state.Accounts, "account-a")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err = reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved := state.Accounts["account-a"]
	if state.Revision != 1 || saved.Task.State != core.TaskDispatchIntent || saved.Task.OperationID != "operation-a" || saved.Task.Intents[0].RandomizedDueAt.IsZero() || saved.Task.CoveredEventIDs[0] != "event-a" || saved.FiveHour.Remaining.Value != 37 || !state.Reservations[0].Sent {
		t.Fatalf("lost durable dispatch state: %#v", state)
	}
	for name, perm := range map[string]os.FileMode{"state.json": 0600, ".lock": 0600, ".": 0700} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != perm {
			t.Fatalf("%s permissions = %v, %v", name, info, err)
		}
	}
}

func TestRevisionCASDoesNotOverwrite(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), 0, core.CoreState{}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale commit = %v", err)
	}
	state, err := store.Load(context.Background())
	if err != nil || len(state.Accounts) != 1 || state.Revision != 1 {
		t.Fatalf("conflict changed state: %#v, %v", state, err)
	}
	if revision, err := store.Commit(context.Background(), 1, state); err != nil || revision != 2 {
		t.Fatalf("next revision = %d, %v", revision, err)
	}
}

func TestConcurrentCASAllowsOneWriter(t *testing.T) {
	store, _ := openTestStore(t)
	var workers sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := store.Commit(context.Background(), 0, dispatchState())
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("unexpected concurrent commit error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successful writers, want 1", successes)
	}
	state, err := store.Load(context.Background())
	if err != nil || state.Revision != 1 || len(state.Accounts) != 1 {
		t.Fatalf("concurrent state = %#v, %v", state, err)
	}
}

func TestProcessLock(t *testing.T) {
	store, dir := openTestStore(t)
	second, err := Open(dir)
	if err == nil {
		second.Close()
		t.Fatal("second Open acquired process lock")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageSubprocess$")
	cmd.Env = append(os.Environ(), "STORAGE_TEST_MODE=locked", "STORAGE_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess lock check: %s, %v", out, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestCrashRetainsDispatchIntent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageSubprocess$")
	cmd.Env = append(os.Environ(), "STORAGE_TEST_MODE=crash", "STORAGE_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %s, %v", out, err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	task := state.Accounts["account-a"].Task
	if state.Revision != 1 || task == nil || task.State != core.TaskDispatchIntent || task.OperationID != "operation-a" || task.DispatchIntentAt.IsZero() {
		t.Fatalf("lost send suppression on crash: %#v", state)
	}
}

func TestStorageSubprocess(t *testing.T) {
	mode := os.Getenv("STORAGE_TEST_MODE")
	if mode == "" {
		return
	}
	store, err := Open(os.Getenv("STORAGE_TEST_DIR"))
	if mode == "locked" {
		if err == nil {
			store.Close()
			t.Fatal("subprocess acquired held lock")
		}
		return
	}
	if mode != "crash" {
		t.Fatalf("unknown test mode %q", mode)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
		t.Fatal(err)
	}
	// Simulate a process exit before Close, as after a durable send intent but
	// before receiving any send result. The OS must release the process lock.
	os.Exit(0)
}

func TestExternalChangesFailClosed(t *testing.T) {
	for _, mutation := range []string{"rewrite", "delete", "create", "replace-lock"} {
		t.Run(mutation, func(t *testing.T) {
			store, dir := openTestStore(t)
			if mutation != "create" {
				if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "rewrite", "create":
				altered := dispatchState()
				altered.Revision = 1
				altered.Accounts["extra"] = core.AccountState{ID: "extra"}
				data, _ := json.Marshal(envelope{Schema: 1, State: &altered})
				if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
					t.Fatal(err)
				}
			case "replace-lock":
				if err := os.Rename(filepath.Join(dir, ".lock"), filepath.Join(dir, ".old-lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Load(context.Background()); err == nil {
				t.Fatal("Load accepted external modification")
			}
			before, _ := os.ReadFile(filepath.Join(dir, "state.json"))
			if _, err := store.Commit(context.Background(), 1, core.CoreState{}); err == nil {
				t.Fatal("Commit accepted external modification")
			}
			after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
			if string(after) != string(before) {
				t.Fatal("failed commit overwrote external state")
			}
		})
	}
}

func TestInvalidStateIsNeverReset(t *testing.T) {
	for _, data := range []string{
		``, `{`, `{"schema":2,"state":{}}`, `{"schema":1}`, `{"schema":1,"state":null}`,
		`{"schema":1,"state":{"Revision":"bad"}}`, `{"schema":1,"state":{}} {}`,
		`{"schema":1,"state":{},"unrecognized":true}`,
	} {
		t.Run(data, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir)
			if err == nil {
				store.Close()
				t.Fatal("opened corrupt/incompatible state")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != data {
				t.Fatal("Open changed invalid state")
			}
			// Invalid Open must release its process lock.
			if err := os.WriteFile(path, []byte(`{"schema":1,"state":{}}`), 0600); err != nil {
				t.Fatal(err)
			}
			store, err = Open(dir)
			if err != nil {
				t.Fatalf("failed Open leaked lock: %v", err)
			}
			store.Close()
		})
	}
}

func TestStaleTemporaryFilesAreIgnored(t *testing.T) {
	store, dir := openTestStore(t)
	if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".state-unfinished.tmp")
	if err := os.WriteFile(tmp, []byte(`{"schema":1,"state":{"Revision":999}}`), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.Load(context.Background())
	if err != nil || state.Revision != 1 {
		t.Fatalf("temporary file used as state: %#v, %v", state, err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("Open removed preexisting temporary file")
	}
}

func TestCancellationAndEncodingFailureDoNotChangeRevision(t *testing.T) {
	store, _ := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Load = %v", err)
	}
	if _, err := store.Commit(ctx, 0, dispatchState()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Commit = %v", err)
	}
	invalid := dispatchState()
	invalid.Accounts["account-a"].FiveHour.Remaining.Value = math.NaN()
	if _, err := store.Commit(context.Background(), 0, invalid); err == nil {
		t.Fatal("NaN encoded successfully")
	}
	state, err := store.Load(context.Background())
	if err != nil || state.Revision != 0 {
		t.Fatalf("failed commit changed revision: %#v, %v", state, err)
	}
	if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorySyncFailureHasUncertainCommitAndStopsWriter(t *testing.T) {
	store, dir := openTestStore(t)
	failure := errors.New("injected directory sync failure")
	store.syncDirectory = func(*os.File) error { return failure }
	revision, err := store.Commit(context.Background(), 0, dispatchState())
	if revision != 0 || !errors.Is(err, failure) {
		t.Fatalf("uncertain commit = %d, %v", revision, err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("poisoned Load = %v", err)
	}
	if _, err := store.Commit(context.Background(), 0, core.CoreState{}); !errors.Is(err, failure) {
		t.Fatalf("poisoned Commit = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.Load(context.Background())
	if err != nil || state.Revision != 1 || state.Accounts["account-a"].Task == nil {
		t.Fatalf("recovery lost potentially committed send intent: %#v, %v", state, err)
	}
}

func TestInsecureAndReadOnlyPermissionsFailClosed(t *testing.T) {
	for _, mode := range []os.FileMode{0755, 0500} {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatal(err)
		}
		store, err := Open(dir)
		if err == nil {
			store.Close()
			t.Fatalf("accepted directory mode %o", mode)
		}
	}
	store, dir := openTestStore(t)
	if _, err := store.Commit(context.Background(), 0, dispatchState()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), 1, core.CoreState{}); err == nil {
		t.Fatal("accepted read-only state")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved envelope
	if err := json.Unmarshal(data, &saved); err != nil || saved.State.Revision != 1 {
		t.Fatal("read-only state was overwritten")
	}
}

func TestStateSymlinkRejectedWithoutTouchingTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	original := []byte(`{"schema":1,"state":{}}`)
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err == nil {
		store.Close()
		t.Fatal("accepted state symlink")
	}
	after, _ := os.ReadFile(target)
	if string(after) != string(original) {
		t.Fatal("changed symlink target")
	}
}

func TestClosedMethods(t *testing.T) {
	store, _ := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Load = %v", err)
	}
	if _, err := store.Commit(context.Background(), 0, core.CoreState{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Commit = %v", err)
	}
}
