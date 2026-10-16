package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionOneMigratesWithoutLosingJobOrIdempotency(t *testing.T) {
	dir := privateDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(schema, "CREATE INDEX IF NOT EXISTS jobs_retention")[0]
	old = strings.Replace(old, "SELECT 3 WHERE NOT EXISTS", "SELECT 1 WHERE NOT EXISTS", 1)
	if _, err = db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES('original','key','hash','a','queued',?,?)", now(), now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, existing, err := s.Create(context.Background(), "key", "hash", "a")
	if err != nil || !existing || id != "original" {
		t.Fatal(id, existing, err)
	}
	var version int
	if err = s.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != 3 {
		t.Fatal(version, err)
	}
}

func TestVersionTwoMigrationKeepsQuotaAndAddsBindingState(t *testing.T) {
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
	old := strings.Split(schema, "CREATE TABLE IF NOT EXISTS account_bindings")[0]
	old = strings.Replace(old, "SELECT 3 WHERE NOT EXISTS", "SELECT 2 WHERE NOT EXISTS", 1)
	if _, err = db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO accounts(id,auth_index,status,quota,updated_at) VALUES('a','index','available','{"permission":"allowed"}',?)`, now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	accounts, err := s.Accounts(context.Background(), "", 10)
	if err != nil || len(accounts) != 1 || string(accounts[0].Quota) != `{"permission":"allowed"}` {
		t.Fatal(accounts, err)
	}
	if _, err = s.Overview(context.Background()); err != nil {
		t.Fatal(err)
	}
}
