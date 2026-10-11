package core

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func assertActivityCounts(t *testing.T, s CoreState) {
	t.Helper()
	want := make(map[UserIdentity]int)
	for _, r := range s.Requests {
		if !r.Completed && r.Identity != "" {
			want[r.Identity]++
		}
	}
	for id, u := range s.Activity {
		if u.InFlightCount != want[id] {
			t.Fatalf("identity %q occupancy=%d, request evidence=%d", id, u.InFlightCount, want[id])
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("unfinished requests without activity entries: %v", want)
	}
}

func TestActivityIncrementalCountsWithConcurrentRequestsAndRetries(t *testing.T) {
	s, now := reducerState()
	events := []ActivityEvent{
		{RequestID: "r1", Phase: ActivityStarted, Identity: "u1"},
		{RequestID: "r2", Phase: ActivityStarted, Identity: "u1"},
		{RequestID: "r1", Phase: ActivityStarted, Identity: "ignored-retry-identity"},
		{RequestID: "r1", Phase: ActivityAccountSelected, AccountID: "a", Generation: "g1"},
		{RequestID: "r1", Phase: ActivityAccountSelected, AccountID: "b", Generation: "g1"},
		{RequestID: "r3", Phase: ActivityAccountSelected, Identity: "u2", AccountID: "a", Generation: "g1"},
		{RequestID: "unknown", Phase: ActivityCompleted, Identity: "u3"},
		{RequestID: "r4", Phase: ActivityCompletionOnly, Identity: "u3"},
		{RequestID: "r1", Phase: ActivityCompleted},
		{RequestID: "r1", Phase: ActivityCompleted},
		{RequestID: "r1", Phase: ActivityStarted, Identity: "u1"},
		{RequestID: "r2", Phase: ActivityCompleted},
	}
	for i, event := range events {
		event.At = now.Add(time.Duration(i) * time.Second)
		ApplyActivity(&s, event)
		assertActivityCounts(t, s)
	}
	if s.Activity["u1"].InFlightCount != 0 || s.Activity["u2"].InFlightCount != 1 || s.Activity["u3"].InFlightCount != 0 {
		t.Fatal("incorrect final occupancy")
	}
	if _, found := s.Activity["ignored-retry-identity"]; found {
		t.Fatal("retry changed authenticated identity")
	}
	RecoverAfterRestart(&s, now.Add(time.Minute))
	assertActivityCounts(t, s)
	ApplyActivity(&s, ActivityEvent{RequestID: "r3", Phase: ActivityCompleted, At: now.Add(2 * time.Minute)})
	assertActivityCounts(t, s)
}

func TestActivityIncrementalCountsWithOutOfOrderReplay(t *testing.T) {
	s, now := reducerState()
	rng := rand.New(rand.NewSource(7))
	phases := []ActivityPhase{ActivityStarted, ActivityAccountSelected, ActivityCompleted, ActivityCompletionOnly, "invalid"}
	for i := 0; i < 3000; i++ {
		event := ActivityEvent{
			RequestID: RequestID(fmt.Sprintf("request-%d", rng.Intn(100))),
			Identity:  UserIdentity(fmt.Sprintf("user-%d", rng.Intn(12))),
			Phase:     phases[rng.Intn(len(phases))],
			At:        now.Add(time.Duration(i+rng.Intn(31)-15) * time.Second),
			AccountID: "a", Generation: "g1",
		}
		if rng.Intn(5) == 0 {
			event.Identity = ""
		}
		ApplyActivity(&s, event)
		assertActivityCounts(t, s)
	}
}

// Retained completed callbacks are the common large-pool case. Processing an
// active request should cost the same with one thousand or one hundred thousand
// tombstones; none of those tombstones can affect current occupancy.
func BenchmarkApplyActivityRetainedHistory(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("completed_%d", count), func(b *testing.B) {
			s := CoreState{Requests: make(map[RequestID]RequestActivity, count+1), Activity: make(map[UserIdentity]UserActivity)}
			now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
			for i := 0; i < count; i++ {
				s.Requests[RequestID(fmt.Sprintf("old-%d", i))] = RequestActivity{Identity: "history", Completed: true, LastEventAt: now}
			}
			s.Activity["history"] = UserActivity{Identity: "history", LastActivity: now}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				delete(s.Requests, "active")
				ApplyActivity(&s, ActivityEvent{RequestID: "active", Phase: ActivityStarted, Identity: "current", At: now})
				ApplyActivity(&s, ActivityEvent{RequestID: "active", Phase: ActivityAccountSelected, AccountID: "a", Generation: "g1", At: now})
				ApplyActivity(&s, ActivityEvent{RequestID: "active", Phase: ActivityCompleted, At: now})
			}
		})
	}
}
