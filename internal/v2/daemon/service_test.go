package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type slowSource struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowSource) Accounts(context.Context) ([]store.Account, error) {
	return []store.Account{{ID: "a", AuthIndex: "a", Status: "available"}, {ID: "b", AuthIndex: "b", Status: "available"}}, nil
}
func (s *slowSource) Query(ctx context.Context, a store.Account) (json.RawMessage, error) {
	if a.ID == "a" {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
	}
	return json.RawMessage(`{"permission":"allowed"}`), nil
}
func request(s *Service, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func accepted(t *testing.T, s *Service, target, key string) string {
	t.Helper()
	w := request(s, "POST", "/api/v2/jobs", `{"kind":"quota_query","account_id":"`+target+`"}`, key)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		ID string `json:"job_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ID == "" {
		t.Fatal("missing job ID")
	}
	return result.ID
}
func until(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deadline waiting for task")
}
func TestSlowAccountDoesNotBlockOtherQueryOrControlAPI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := &slowSource{entered: make(chan struct{}), release: make(chan struct{})}
	s, err := New(st, source, Options{QueryConcurrency: 2, QueryTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	a := accepted(t, s, "a", "one")
	select {
	case <-source.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("query not started")
	}
	b := accepted(t, s, "b", "two")
	until(t, func() bool { j, e := st.Job(ctx, b); return e == nil && j.Phase == "completed" })
	if w := request(s, "GET", "/api/v2/overview", "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if again := accepted(t, s, "a", "one"); again != a {
		t.Fatal("duplicate submit changed job")
	}
	close(source.release)
	until(t, func() bool { j, e := st.Job(ctx, a); return e == nil && j.Phase == "completed" })
}

type failedSource struct{}

func (failedSource) Accounts(context.Context) ([]store.Account, error) {
	return nil, errors.New("bridge_unavailable")
}
func (failedSource) Query(context.Context, store.Account) (json.RawMessage, error) {
	panic("unreachable")
}
func TestAPIRejectsSideEffectsAndUnknownFields(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := New(st, failedSource{}, Options{QueryConcurrency: 8, QueryTimeout: time.Second})
	for _, body := range []string{`{"kind":"wake"}`, `{"kind":"quota_query","prompt":"secret"}`, `{"kind":"quota_query"} {}`} {
		if w := request(s, http.MethodPost, "/api/v2/jobs", body, "key"); w.Code != 400 {
			t.Fatal("accepted invalid command", body)
		}
	}
	if w := request(s, "POST", "/api/v2/jobs", `{"kind":"quota_query"}`, ""); w.Code != 400 {
		t.Fatal("accepted non-idempotent submission")
	}
	id := accepted(t, s, "a", "key")
	if w := request(s, "GET", "/api/v2/jobs?job_id="+id, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request(s, "POST", "/api/v2/jobs", `{"kind":"quota_query","account_id":"b"}`, "key"); w.Code != 409 {
		t.Fatal("conflicting idempotency key accepted")
	}
}

func TestAccountAPIRequiresConsistentPaginationAndReportsActivityScope(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := New(st, failedSource{}, Options{QueryConcurrency: 8, QueryTimeout: time.Second})
	id := accepted(t, s, "", "pages")
	j, _ := st.Job(context.Background(), id)
	if err = st.Expand(context.Background(), j, []store.Account{{ID: "a", AuthIndex: "a", Status: "available"}, {ID: "b", AuthIndex: "b", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	w := request(s, "GET", "/api/v2/accounts?limit=1", "", "")
	var page store.AccountPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Next != "a" || page.InstanceID == "" {
		t.Fatal(w.Body.String())
	}
	if w = request(s, "GET", "/api/v2/accounts?after=a", "", ""); w.Code != 400 {
		t.Fatal("pagination without a revision was accepted")
	}
	path := "/api/v2/accounts?after=a&revision=" + strconv.FormatUint(page.AccountsRevision, 10)
	if w = request(s, "GET", path, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	run, err := st.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Finish(context.Background(), run, []byte(`{"permission":"allowed"}`), ""); err != nil {
		t.Fatal(err)
	}
	if w = request(s, "GET", path, "", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "view_changed") {
		t.Fatal("mixed quota pages accepted", w.Body.String())
	}
	w = request(s, "GET", "/api/v2/overview", "", "")
	var view struct {
		Instance string `json:"instance_id"`
		store.Overview
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Instance != page.InstanceID || view.Revision <= page.Revision || view.Activity.BaselineComplete {
		t.Fatal(w.Body.String())
	}
}

func TestPrunedJobReturnsGoneRatherThanRepeatingAcceptedOperation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := New(st, failedSource{}, Options{QueryConcurrency: 8, QueryTimeout: time.Second})
	id := accepted(t, s, "", "expired")
	j, _ := st.Job(context.Background(), id)
	if err = st.Expand(context.Background(), j, nil); err != nil {
		t.Fatal(err)
	}
	if err = st.Prune(context.Background(), time.Now().Add(8*24*time.Hour), store.DefaultRetention()); err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/api/v2/jobs", `{"kind":"quota_query","account_id":""}`, "expired")
	if w.Code != 410 || !strings.Contains(w.Body.String(), "job_expired") {
		t.Fatal(w.Code, w.Body.String())
	}
}
