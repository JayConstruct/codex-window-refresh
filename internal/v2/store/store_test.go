package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func privateDir(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "state") }
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(privateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func createTest(t *testing.T, s *Store, key, target string) Job {
	t.Helper()
	id, _, err := s.Create(context.Background(), key, "hash-"+target, target)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestIdempotencyAndExclusiveOwnership(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	id, existing, err := s.Create(ctx, "key", "a", "a")
	if err != nil || existing {
		t.Fatal(err)
	}
	second, existing, err := s.Create(ctx, "key", "a", "a")
	if err != nil || !existing || second != id {
		t.Fatal("idempotency did not preserve identity")
	}
	if _, _, err = s.Create(ctx, "key", "b", "b"); !errors.Is(err, ErrConflict) {
		t.Fatal("different body reused key")
	}
}
func TestCoalescedQueryCommitsAllParentsAndEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	account := Account{ID: "a", AuthIndex: "idx", Status: "available"}
	first := createTest(t, s, "one", "a")
	if err := s.Expand(ctx, first, []Account{account}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := createTest(t, s, "two", "a")
	if err = s.Expand(ctx, second, []Account{account}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("same account ran twice")
	}
	if err = s.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal("commit acknowledgement retry was not idempotent", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		j, e := s.Job(ctx, id)
		if e != nil || j.Phase != "completed" || len(j.Items) != 1 || j.Items[0].Phase != "completed" {
			t.Fatalf("parent=%+v err=%v", j, e)
		}
	}
	accounts, err := s.Accounts(ctx, "", 10)
	if err != nil || len(accounts) != 1 || len(accounts[0].Quota) == 0 {
		t.Fatal("successful job missing committed quota")
	}
	if err = s.Finish(ctx, run, []byte(`{}`), ""); err == nil {
		t.Fatal("stale completion accepted")
	}
}

func TestAccountReplacementCannotAcceptOldQueryOrJoinItsRun(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	first := createTest(t, s, "old", "a")
	if err := s.Expand(ctx, first, []Account{{ID: "a", AuthIndex: "old", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	old, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := createTest(t, s, "new", "a")
	if err = s.Expand(ctx, next, []Account{{ID: "a", AuthIndex: "new", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("replacement bypassed physical account slot")
	}
	if err = s.Finish(ctx, old, []byte(`{"old":true}`), ""); err != nil {
		t.Fatal(err)
	}
	previous, _ := s.Job(ctx, first.ID)
	if previous.Phase != "failed" || previous.Items[0].ErrorCode != "account_binding_changed" {
		t.Fatal("old result accepted")
	}
	current, err := s.Claim(ctx)
	if err != nil || current.Account.AuthIndex != "new" {
		t.Fatal("replacement query not scheduled", err)
	}
	if err = s.Finish(ctx, current, []byte(`{"new":true}`), ""); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.Accounts(ctx, "", 10)
	if err != nil || len(accounts) != 1 || string(accounts[0].Quota) != `{"new":true}` {
		t.Fatal("stale quota published")
	}
}

func TestForeignSchemaIsRejectedBeforeCreatingApplicationTables(t *testing.T) {
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE schema_version(version); INSERT INTO schema_version VALUES(99); CREATE TABLE preserve(value); INSERT INTO preserve VALUES(73)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("newer schema accepted")
	}
	db, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count, value int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='accounts'").Scan(&count); err != nil || count != 0 {
		t.Fatal("foreign schema changed")
	}
	if err = db.QueryRow("SELECT value FROM preserve").Scan(&value); err != nil || value != 73 {
		t.Fatal("foreign data changed")
	}
}
func TestAcceptedAndRunningReadsRecoverAcrossRestart(t *testing.T) {
	dir := privateDir(t)
	ctx := context.Background()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := createTest(t, s, "restart", "a")
	if err = s.Expand(ctx, j, []Account{{ID: "a", AuthIndex: "i", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	old, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.Claim(ctx)
	if err != nil || recovered.ID == old.ID {
		t.Fatalf("recover: %v", err)
	}
	if err = s.Finish(ctx, old, []byte(`{}`), ""); err == nil {
		t.Fatal("old worker result accepted")
	}
	if err = s.Finish(ctx, recovered, []byte(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	result, err := s.Job(ctx, j.ID)
	if err != nil || result.Phase != "completed" {
		t.Fatal("original job not recovered")
	}
}
func TestUnsafeDatabasePathRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "backend.sqlite")); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("symlink accepted")
	}
	raw, _ := os.ReadFile(outside)
	if string(raw) != "preserve" {
		t.Fatal("foreign data changed")
	}
}
