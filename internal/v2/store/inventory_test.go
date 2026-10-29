package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func inventoryAccounts(count int) []Account {
	accounts := make([]Account, count)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("account-%04d", i), AuthIndex: fmt.Sprintf("index-%04d", i), Binding: fmt.Sprintf("binding-%04d", i), Status: "available"}
	}
	return accounts
}

func inventoryWriteAudit(t testing.TB, storage *Store) {
	t.Helper()
	_, err := storage.db.Exec(`CREATE TABLE inventory_write_audit(kind TEXT NOT NULL);
CREATE TRIGGER audit_inventory_account AFTER UPDATE ON accounts BEGIN INSERT INTO inventory_write_audit VALUES('account'); END;
CREATE TRIGGER audit_inventory_binding AFTER UPDATE ON account_bindings BEGIN INSERT INTO inventory_write_audit VALUES('binding'); END;
CREATE TRIGGER audit_inventory_heartbeat AFTER UPDATE ON inventory_state BEGIN INSERT INTO inventory_write_audit VALUES('heartbeat'); END;`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedInventoryWritesOnlyTheSharedHeartbeat(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	accounts := inventoryAccounts(1000)
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	before, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	var original, bindingCheckpoint string
	if err = storage.db.QueryRow("SELECT updated_at,confirmed_at FROM accounts JOIN account_bindings ON account_id=id WHERE id=?", accounts[0].ID).Scan(&original, &bindingCheckpoint); err != nil {
		t.Fatal(err)
	}
	inventoryWriteAudit(t, storage)
	if err = storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	var accountWrites, bindingWrites, heartbeatWrites int
	if err = storage.db.QueryRow("SELECT COALESCE(sum(kind='account'),0),COALESCE(sum(kind='binding'),0),COALESCE(sum(kind='heartbeat'),0) FROM inventory_write_audit").Scan(&accountWrites, &bindingWrites, &heartbeatWrites); err != nil {
		t.Fatal(err)
	}
	if accountWrites != 0 || bindingWrites != 0 || heartbeatWrites != 1 {
		t.Fatal("unchanged inventory rewrote account rows", accountWrites, bindingWrites, heartbeatWrites)
	}
	var unchanged, checkpoint, latest string
	if err = storage.db.QueryRow("SELECT updated_at,confirmed_at,(SELECT last_success_at FROM inventory_state) FROM accounts JOIN account_bindings ON account_id=id WHERE id=?", accounts[0].ID).Scan(&unchanged, &checkpoint, &latest); err != nil {
		t.Fatal(err)
	}
	if unchanged != original || checkpoint != bindingCheckpoint || latest <= original {
		t.Fatal("heartbeat and row timestamps were not independent", original, unchanged, bindingCheckpoint, checkpoint, latest)
	}
	after, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || after.Revision != before.Revision || after.AccountsRevision != before.AccountsRevision || after.Accounts[0].StatusObservedAt != latest {
		t.Fatal("unchanged sync changed revisions or lost observed status freshness", after, err)
	}
}

func TestInventoryCheckpointCoalescesButRefreshesOldAndFutureBindings(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	accounts := inventoryAccounts(3)
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{time.Now().Add(-inventoryBindingCheckpointInterval - time.Minute), time.Now().Add(time.Hour)} {
		if _, err := storage.db.Exec("UPDATE account_bindings SET confirmed_at=? WHERE account_id=?", queryStamp(at), accounts[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	inventoryWriteAudit(t, storage)
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	var bindingWrites int
	if err := storage.db.QueryRow("SELECT count(*) FROM inventory_write_audit WHERE kind='binding'").Scan(&bindingWrites); err != nil || bindingWrites != 2 {
		t.Fatal("did not refresh precisely the stale checkpoints", bindingWrites, err)
	}
	var mismatches int
	if err := storage.db.QueryRow("SELECT count(*) FROM account_bindings WHERE account_id IN (?,?) AND confirmed_at!=(SELECT last_success_at FROM inventory_state)", accounts[0].ID, accounts[1].ID).Scan(&mismatches); err != nil || mismatches != 0 {
		t.Fatal("checkpoints disagree with current verification", mismatches, err)
	}
}

func TestUnchangedInventoryHeartbeatFencesOlderQueryStatus(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	run, _ := queryStatusRun(t, storage)
	old := observedQueryStatus(run, "disabled")
	inventoryWriteAudit(t, storage)
	if err := storage.SyncInventory(ctx, []Account{run.Account}); err != nil {
		t.Fatal(err)
	}
	var writes int
	if err := storage.db.QueryRow("SELECT count(*) FROM inventory_write_audit WHERE kind='account'").Scan(&writes); err != nil || writes != 0 {
		t.Fatal("fixture did not exercise unchanged account rows", writes, err)
	}
	if err := storage.FinishWithStatus(ctx, run, nil, "quota_transport_failed", old); err != nil {
		t.Fatal(err)
	}
	page, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || len(page.Accounts) != 1 || page.Accounts[0].Status != "available" || page.Accounts[0].StatusObservedAt <= old.StatusObservedAt {
		t.Fatal("old query status replaced a newer complete inventory observation", page, err)
	}
	// The heartbeat must not prevent later query evidence from updating status.
	job := createTest(t, storage, "newer-status-query", run.Account.ID)
	if err := storage.Expand(ctx, job, []Account{run.Account}); err != nil {
		t.Fatal(err)
	}
	newer, err := storage.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observed := observedQueryStatus(newer, "disabled")
	if err = storage.FinishWithStatus(ctx, newer, nil, "quota_transport_failed", observed); err != nil {
		t.Fatal(err)
	}
	page, err = storage.AccountPage(ctx, "", 10, nil)
	if err != nil || page.Accounts[0].Status != "disabled" || page.Accounts[0].StatusObservedAt != observed.StatusObservedAt {
		t.Fatal("newer query status was not accepted", page, err)
	}
}

func TestInventorySetMergePreservesQuotaOnlyForUnchangedIdentity(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	accounts := inventoryAccounts(5)
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	raw := `{"permission":"allowed"}`
	if _, err := storage.db.Exec("UPDATE accounts SET quota=?,observed_at=?", raw, now()); err != nil {
		t.Fatal(err)
	}
	inventoryWriteAudit(t, storage)
	accounts[1].Status = "disabled"
	accounts[2].AuthIndex = "replacement-index"
	accounts[3].Binding = "replacement-binding"
	removed := accounts[4]
	accounts = accounts[:4]
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	for i, account := range append(accounts, removed) {
		var status, index, quota, observed string
		if err := storage.db.QueryRow("SELECT status,auth_index,COALESCE(quota,''),COALESCE(observed_at,'') FROM accounts WHERE id=?", account.ID).Scan(&status, &index, &quota, &observed); err != nil {
			t.Fatal(err)
		}
		if index != account.AuthIndex || (i < 4 && status != account.Status) || i == 4 && status != "removed" {
			t.Fatal("snapshot did not update identity/status", account, status, index)
		}
		if i < 2 && (quota != raw || observed == "") || i >= 2 && (quota != "" || observed != "") {
			t.Fatal("quota survived changed identity or was erased on status-only update", account, quota, observed)
		}
	}
	var remaining, writes int
	if err := storage.db.QueryRow("SELECT count(*) FROM account_bindings WHERE account_id=?", removed.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("removed account kept its binding", remaining, err)
	}
	if err := storage.db.QueryRow("SELECT count(*) FROM inventory_write_audit WHERE kind='account'").Scan(&writes); err != nil || writes != 4 {
		t.Fatal("merge did not isolate changed rows", writes, err)
	}
}

func TestInventoryInvalidSnapshotsDoNotPublishPartialData(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	accounts := inventoryAccounts(2)
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	before, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]Account{{accounts[0], accounts[0]}, {{ID: "bad\nidentifier"}}} {
		if err := storage.SyncInventory(ctx, invalid); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
		after, err := storage.AccountPage(ctx, "", 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		oldJSON, _ := json.Marshal(before)
		newJSON, _ := json.Marshal(after)
		if string(oldJSON) != string(newJSON) {
			t.Fatal("invalid snapshot changed durable inventory", string(newJSON))
		}
	}
	// Staged rows from the rolled-back duplicate must not affect the next sync.
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSyncUnchangedInventory(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			storage, err := Open(filepath.Join(b.TempDir(), "state"))
			if err != nil {
				b.Fatal(err)
			}
			defer storage.Close()
			ctx := context.Background()
			accounts := inventoryAccounts(count)
			if err = storage.SyncInventory(ctx, accounts); err != nil {
				b.Fatal(err)
			}
			inventoryWriteAudit(b, storage)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err = storage.SyncInventory(ctx, accounts); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			var writes int
			if err = storage.db.QueryRow("SELECT count(*) FROM inventory_write_audit").Scan(&writes); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(writes)/float64(b.N), "main-row-writes/op")
		})
	}
}
