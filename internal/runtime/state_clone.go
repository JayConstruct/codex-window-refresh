package runtime

import (
	"maps"
	"slices"

	"codex-window-refresh/internal/core"
)

// Copy mutable containers, while sharing immutable strings and time locations.
// A JSON round trip duplicated every historical prompt and allocated a separate
// location for every persisted timestamp on each publication and panel read.
func cloneState(state core.CoreState) core.CoreState {
	state.Accounts = maps.Clone(state.Accounts)
	for id, account := range state.Accounts {
		account.FiveHour.Remaining = clonePointer(account.FiveHour.Remaining)
		account.Weekly.Remaining = clonePointer(account.Weekly.Remaining)
		account.Task = clonePointer(account.Task)
		if account.Task != nil {
			account.Task.Intents = slices.Clone(account.Task.Intents)
			account.Task.CoveredEventIDs = slices.Clone(account.Task.CoveredEventIDs)
		}
		state.Accounts[id] = account
	}
	state.DailyIntents = slices.Clone(state.DailyIntents)
	state.Reservations = slices.Clone(state.Reservations)
	state.Activity = maps.Clone(state.Activity)
	state.Requests = maps.Clone(state.Requests)
	state.History = slices.Clone(state.History)
	for index := range state.History {
		state.History[index].Sources = slices.Clone(state.History[index].Sources)
		state.History[index].CoveredEvents = slices.Clone(state.History[index].CoveredEvents)
	}
	state.GenerationJob = clonePointer(state.GenerationJob)
	if state.GenerationJob != nil {
		state.GenerationJob.Candidates = slices.Clone(state.GenerationJob.Candidates)
	}
	state.GenerationAccounts = maps.Clone(state.GenerationAccounts)
	return state
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
