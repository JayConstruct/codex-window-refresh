package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
