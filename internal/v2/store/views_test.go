package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestFilteredAccountPagesFilterBeforeLimitAndCountBeforeCursor(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	accounts := []Account{}
	for index := range 30 {
		status := "available"
		if index%2 == 0 {
			status = "disabled"
		}
		accounts = append(accounts, Account{ID: fmt.Sprintf("Account-%02d", index), AuthIndex: "index", Status: status})
	}
	accounts = append(accounts, Account{ID: "unavailable", Status: "binding_unavailable"})
	if err := st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	first, err := st.AccountPageFiltered(ctx, "", 6, nil, "ACCOUNT-", "disabled")
	if err != nil || len(first.Accounts) != 6 || first.Next != "Account-10" || first.Total != 31 || first.FilterTotal != 15 || first.Offset != 0 {
		t.Fatal(first, err)
	}
	second, err := st.AccountPageFiltered(ctx, first.Next, 6, &first.AccountsRevision, "account-", "disabled")
	if err != nil || len(second.Accounts) != 6 || second.Accounts[0].ID != "Account-12" || second.FilterTotal != 15 || second.Offset != 6 {
		t.Fatal(second, err)
	}
	last, err := st.AccountPageFiltered(ctx, second.Next, 6, &first.AccountsRevision, "account-", "disabled")
	if err != nil || len(last.Accounts) != 3 || last.Next != "" || last.FilterTotal != 15 || last.Offset != 12 {
		t.Fatal(last, err)
	}
	if _, err = st.db.Exec(`UPDATE accounts SET quota='{"permission":"blocked"}' WHERE id='Account-01'`); err != nil {
		t.Fatal(err)
	}
	attention, err := st.AccountPageFiltered(ctx, "", 6, nil, "", "attention")
	if err != nil || attention.FilterTotal != 2 {
		t.Fatal(attention, err)
	}
	literal, err := st.AccountPageFiltered(ctx, "", 6, nil, "%", "all")
	if err != nil || literal.FilterTotal != 0 {
		t.Fatal("search interpreted SQL wildcards", literal, err)
	}
	if _, err = st.AccountPageFiltered(ctx, "", 6, nil, "", "invented"); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestAccountOffsetPreservesCursorUsingNewRevisionAndUpdatedFilter(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	accounts := []Account{}
	for index := range 20 {
		accounts = append(accounts, Account{ID: fmt.Sprintf("account-%02d", index), AuthIndex: "index", Status: "available"})
	}
	if err := st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	first, err := st.AccountPageFiltered(ctx, "", 6, nil, "account-", "available")
	if err != nil {
		t.Fatal(err)
	}
	accounts[0].Status = "disabled"
	accounts[5].Status = "disabled"
	if err = st.SyncInventory(ctx, accounts); err != nil {
		t.Fatal(err)
	}
	if _, err = st.AccountPageFiltered(ctx, first.Next, 6, &first.AccountsRevision, "account-", "available"); !errors.Is(err, ErrViewChanged) {
		t.Fatal("old revision was accepted", err)
	}
	view, err := st.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := st.AccountPageFiltered(ctx, first.Next, 6, &view.AccountsRevision, "account-", "available")
	if err != nil || replacement.Offset != 4 || replacement.FilterTotal != 18 || replacement.Total != 20 || len(replacement.Accounts) != 6 || replacement.Accounts[0].ID != "account-06" {
		t.Fatal(replacement, err)
	}
	empty, err := st.AccountPageFiltered(ctx, "zzzz", 6, &view.AccountsRevision, "account-", "available")
	if err != nil || empty.Offset != 18 || len(empty.Accounts) != 0 || empty.Next != "" {
		t.Fatal(empty, err)
	}
}

func TestAccountPaginationRejectsMixedQuotaVersionsButAllowsJobProgress(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := createTest(t, s, "pages", "")
	if err := s.Expand(ctx, j, []Account{{ID: "a", AuthIndex: "a", Status: "available"}, {ID: "b", AuthIndex: "b", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	first, err := s.AccountPage(ctx, "", 1, nil)
	if err != nil || first.Next != "a" {
		t.Fatal(first, err)
	}
	run, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AccountPage(ctx, first.Next, 1, &first.AccountsRevision)
	if err != nil || len(second.Accounts) != 1 || second.Accounts[0].ID != "b" || second.Revision <= first.Revision {
		t.Fatal(second, err)
	}
	if err = s.Finish(ctx, run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccountPage(ctx, first.Next, 1, &first.AccountsRevision); !errors.Is(err, ErrViewChanged) {
		t.Fatal("mixed-version page accepted", err)
	}
}
func TestPrunedHistoryKeepsIdempotencyReceiptAndActiveJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, key := range []string{"old-one", "old-two", "latest"} {
		j := createTest(t, s, key, "")
		if err := s.Expand(ctx, j, nil); err != nil {
			t.Fatal(err)
		}
	}
	queued := createTest(t, s, "active", "a")
	if err := s.Prune(ctx, time.Now(), Retention{HistoryAge: DefaultRetention().HistoryAge, TerminalJobs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, "old-one", "hash-", ""); !errors.Is(err, ErrJobExpired) {
		t.Fatal("pruned job silently resubmitted", err)
	}
	if _, _, err := s.Create(ctx, "old-one", "different", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("receipt lost request hash", err)
	}
	if j, err := s.Job(ctx, queued.ID); err != nil || j.Phase != "queued" {
		t.Fatal("active job pruned")
	}
}
