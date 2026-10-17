package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAccountStatusProjectionFiltersAndDashboardUseTheSameEvidence(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	cases := []struct {
		id, host, raw, display string
	}{
		{"normal", "available", `{"permission":"allowed","five_hour":{"Remaining":{"Value":80,"Unit":"percent"}}}`, "available"},
		{"short-empty", "available", `{"permission":"allowed","five_hour":{"Remaining":{"Value":0,"Unit":"percent"}}}`, "quota_exhausted"},
		{"week-empty", "available", `{"permission":"blocked","weekly":{"Remaining":{"Value":0,"Unit":"percent"}}}`, "quota_exhausted"},
		{"restricted", "available", `{"permission":"blocked"}`, "quota_restricted"},
		{"disabled-empty", "disabled", `{"five_hour":{"Remaining":{"Value":0,"Unit":"percent"}}}`, "disabled"},
		{"unavailable-empty", "unavailable", `{"five_hour":{"Remaining":{"Value":0,"Unit":"percent"}}}`, "unavailable"},
		{"identity-empty", "binding_unavailable", `{"five_hour":{"Remaining":{"Value":0,"Unit":"percent"}}}`, "binding_unavailable"},
		{"unknown-quota", "available", ``, "available"},
		{"malformed", "available", `not json`, "available"},
		{"string-zero", "available", `{"five_hour":{"Remaining":{"Value":"0","Unit":"percent"}}}`, "available"},
		{"boolean-zero", "available", `{"five_hour":{"Remaining":{"Value":false,"Unit":"percent"}}}`, "available"},
		{"no-unit", "available", `{"five_hour":{"Remaining":{"Value":0}}}`, "available"},
	}
	accounts := []Account{}
	for _, test := range cases {
		accounts = append(accounts, Account{ID: test.id, AuthIndex: test.id, Binding: "binding", Status: test.host})
	}
	if err := storage.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		if _, err := storage.db.Exec("UPDATE accounts SET quota=? WHERE id=?", test.raw, test.id); err != nil {
			t.Fatal(err)
		}
	}
	page, err := storage.AccountPage(ctx, "", 100, nil)
	if err != nil || len(page.Accounts) != len(cases) {
		t.Fatal(page, err)
	}
	for _, account := range page.Accounts {
		for _, test := range cases {
			if test.id == account.ID && (account.Status != test.host || account.DisplayStatus != test.display || account.StatusObservedAt == "") {
				t.Fatal("host admission status was changed or display status disagrees", account, test)
			}
		}
	}
	for status, expected := range map[string]int{"available": 6, "disabled": 1, "quota_exhausted": 2, "quota_restricted": 1, "unavailable": 2, "attention": 5} {
		filtered, err := storage.AccountPageFiltered(ctx, "", 1, nil, "", status)
		if err != nil || filtered.FilterTotal != expected || len(filtered.Accounts) != 1 {
			t.Fatal(status, filtered, err)
		}
	}
	dashboard, err := storage.Dashboard(ctx)
	if err != nil || dashboard.Normal != 6 || dashboard.QuotaExhausted != 2 || dashboard.QuotaRestricted != 1 || dashboard.Available != 9 || dashboard.Disabled != 1 || dashboard.Unavailable != 2 || dashboard.Total != 12 {
		t.Fatal(dashboard, err)
	}
	var internalStatus string
	if err := storage.db.QueryRow("SELECT status FROM accounts WHERE id='short-empty'").Scan(&internalStatus); err != nil || internalStatus != "available" {
		t.Fatal("display projection changed model/query admission", internalStatus, err)
	}
}

func TestAccountStatusDoesNotResetWithoutNewEvidenceAndRecoversOnReadOnlyUpdate(t *testing.T) {
	storage := openTest(t)
	ctx := context.Background()
	account := Account{ID: "account", AuthIndex: "index", Binding: "binding", Status: "available"}
	if err := storage.SyncInventory(ctx, []Account{account}); err != nil {
		t.Fatal(err)
	}
	depleted := `{"permission":"blocked","five_hour":{"ResetAt":"2000-01-01T00:00:00Z","Remaining":{"Value":0,"Unit":"percent"}}}`
	if _, err := storage.db.Exec("UPDATE accounts SET quota=? WHERE id=?", depleted, account.ID); err != nil {
		t.Fatal(err)
	}
	first, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || first.Accounts[0].DisplayStatus != "quota_exhausted" {
		t.Fatal(first, err)
	}
	for _, host := range []string{"disabled", "available"} {
		account.Status = host
		if err = storage.SyncInventory(ctx, []Account{account}); err != nil {
			t.Fatal(err)
		}
		page, err := storage.AccountPage(ctx, "", 10, nil)
		if err != nil || string(page.Accounts[0].Quota) != depleted || host == "disabled" && page.Accounts[0].DisplayStatus != "disabled" || host == "available" && page.Accounts[0].DisplayStatus != "quota_exhausted" {
			t.Fatal(page, err)
		}
	}
	if _, err = storage.db.Exec(`UPDATE accounts SET quota='{"permission":"allowed","five_hour":{"Remaining":{"Value":100,"Unit":"percent"}}}' WHERE id=?`, account.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := storage.AccountPage(ctx, "", 10, nil)
	if err != nil || updated.Accounts[0].DisplayStatus != "available" || updated.AccountsRevision <= first.AccountsRevision {
		t.Fatal(updated, err)
	}
	raw, _ := json.Marshal(updated.Accounts[0])
	var public map[string]any
	if json.Unmarshal(raw, &public) != nil || public["auth_index"] != nil || public["binding"] != nil {
		t.Fatal("private identity leaked", string(raw))
	}
}
