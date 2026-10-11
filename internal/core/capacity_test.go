package core

import (
	"math"
	"testing"
)

func TestCapacityDeduplicatesAndSubtractsReservations(t *testing.T) {
	got, err := CalculateCapacity(CapacityInput{
		RecentUsers: 4, AccountsPerUser: 0.5,
		Participants: []AccountID{"a", "a", "b", "c"},
		Active:       []AccountID{"a", "a", "external"},
		Reserved:     []AccountID{"a", "b", "b", "external"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Capacity{Participants: 3, UserTarget: 2, Target: 2, Active: 1, Reserved: 1, Missing: 0}
	if got != want {
		t.Fatalf("got %+v; want %+v", got, want)
	}
}

func TestCapacityTargets(t *testing.T) {
	zero, two := 0, 2
	tests := []struct {
		name            string
		in              CapacityInput
		target, missing int
	}{
		{"round up fractional demand", CapacityInput{RecentUsers: 3, AccountsPerUser: 0.5}, 2, 2},
		{"no users no ordinary demand", CapacityInput{AccountsPerUser: 1}, 0, 0},
		{"daily prepares without users", CapacityInput{AccountsPerUser: 1, DailyMinimum: 1}, 1, 1},
		{"cap applies to daily target", CapacityInput{RecentUsers: 8, AccountsPerUser: 1, DailyMinimum: 3, MaxAccounts: &two}, 2, 2},
		{"zero cap disables", CapacityInput{RecentUsers: 8, AccountsPerUser: 1, DailyMinimum: 3, MaxAccounts: &zero}, 0, 0},
		{"demand bounded by pool", CapacityInput{RecentUsers: 8, AccountsPerUser: 1}, 3, 3},
		{"overflow clamps before conversion", CapacityInput{RecentUsers: 10, AccountsPerUser: math.MaxFloat64}, 3, 3},
		{"active capacity above target", CapacityInput{RecentUsers: 1, AccountsPerUser: 1, Active: []AccountID{"a", "b", "c"}}, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Participants = []AccountID{"a", "b", "c"}
			got, err := CalculateCapacity(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Target != tc.target || got.Missing != tc.missing {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestCapacityRejectsInvalidPolicy(t *testing.T) {
	negative := -1
	for _, in := range []CapacityInput{
		{RecentUsers: -1}, {AccountsPerUser: -0.1}, {AccountsPerUser: math.NaN()},
		{AccountsPerUser: math.Inf(1)}, {DailyMinimum: -1}, {MaxAccounts: &negative},
		{Participants: []AccountID{""}},
	} {
		if _, err := CalculateCapacity(in); err == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
}
