package store

import (
	"context"
	"sync"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func TestTaskHistoryTransactionSnapshotWithConcurrentWriter(t *testing.T) {
	storage := openTest(t)
	stamp := now()
	if _, err := storage.db.Exec(`INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES('before','before','hash','a','completed',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	initial, err := storage.TaskHistory(context.Background(), TaskHistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := storage.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var writeErr error
	err = connection.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(operation int, first, second, third string) int {
			switch operation {
			case sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE, sqlite3.SQLITE_CREATE_TABLE, sqlite3.SQLITE_CREATE_INDEX, sqlite3.SQLITE_DROP_TABLE, sqlite3.SQLITE_DROP_INDEX, sqlite3.SQLITE_ALTER_TABLE:
				return sqlite3.SQLITE_DENY
			case sqlite3.SQLITE_READ:
				if first == "jobs" {
					once.Do(func() {
						_, writeErr = storage.db.Exec(`INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,created_at,updated_at) VALUES('concurrent','concurrent','hash','a','completed',?,?)`, stamp, stamp)
					})
				}
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	page, err := storage.TaskHistory(context.Background(), TaskHistoryFilter{})
	if err != nil || writeErr != nil || page.Revision != initial.Revision || page.Total != 1 || len(page.Records) != 1 || page.Records[0].JobID != "before" || page.StatusCounts.All != 1 {
		t.Fatal("count/list/revision not same snapshot", page, err, writeErr)
	}
	page, err = storage.TaskHistory(context.Background(), TaskHistoryFilter{})
	if err != nil || page.Total != 2 || page.Revision <= initial.Revision || page.StatusCounts.All != 2 {
		t.Fatal("new snapshot missed committed writer", page, err)
	}
}
