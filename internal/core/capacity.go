package core

import (
	"fmt"
	"math"
)

type CapacityInput struct {
	RecentUsers     int
	AccountsPerUser float64
	// Only distinct ordinary participants count toward this capacity target.
	Participants []AccountID
	Active       []AccountID
	Reserved     []AccountID
	// Nil means no configured cap; zero explicitly disables ordinary capacity.
	MaxAccounts  *int
	DailyMinimum int
}

type Capacity struct {
	Participants int `json:"participants"`
	UserTarget   int `json:"user_target"`
	Target       int `json:"target"`
	Active       int `json:"active"`
	Reserved     int `json:"reserved"`
	Missing      int `json:"missing"`
}

// CalculateCapacity counts distinct accounts, excludes A∩P from P, and includes
// weekly reservations when their account belongs to the ordinary participant
// set. It does not mutate reservations or account states.
func CalculateCapacity(in CapacityInput) (Capacity, error) {
	if in.RecentUsers < 0 || in.DailyMinimum < 0 || in.AccountsPerUser < 0 ||
		math.IsNaN(in.AccountsPerUser) || math.IsInf(in.AccountsPerUser, 0) ||
		(in.MaxAccounts != nil && *in.MaxAccounts < 0) {
		return Capacity{}, fmt.Errorf("invalid capacity input")
	}
	participants := make(map[AccountID]bool)
	for _, id := range in.Participants {
		if id == "" {
			return Capacity{}, fmt.Errorf("empty participant identity")
		}
		participants[id] = true
	}
	limit := len(participants)
	if in.MaxAccounts != nil {
		limit = min(limit, *in.MaxAccounts)
	}
	product := float64(in.RecentUsers) * in.AccountsPerUser
	userTarget := limit
	// Clamp before conversion; a finite ratio can still overflow the product.
	if product < float64(limit) {
		userTarget = int(math.Ceil(product))
	}
	target := min(limit, max(userTarget, in.DailyMinimum))
	active := make(map[AccountID]bool)
	for _, id := range in.Active {
		if participants[id] {
			active[id] = true
		}
	}
	reserved := make(map[AccountID]bool)
	for _, id := range in.Reserved {
		if participants[id] && !active[id] {
			reserved[id] = true
		}
	}
	return Capacity{
		Participants: len(participants), UserTarget: userTarget, Target: target,
		Active: len(active), Reserved: len(reserved),
		Missing: max(0, target-len(active)-len(reserved)),
	}, nil
}
