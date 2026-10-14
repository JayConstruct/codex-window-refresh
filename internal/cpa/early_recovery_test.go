package cpa

import (
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestRealQuotaParserAndReducerRecognizeFullRecoveryWithUnchangedOrMovingTimes(t *testing.T) {
	for _, moving := range []bool{false, true} {
		at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
		state := core.CoreState{}
		core.ReconcileAccounts(&state, []core.HostAccount{{ID: "a", Generation: "g", Enabled: true}}, at)
		permission := core.PermissionAllowed
		shortReset, weekReset := at.Add(time.Hour), at.Add(24*time.Hour)
		core.ApplyObservation(&state, core.QuotaObservation{AccountID: "a", Generation: "g", Class: core.ObservationQuery, ReadStartedSeq: core.NextSequence(&state), ReceivedAt: at, Permission: &permission, FiveHour: &core.WindowEvidence{State: core.WindowActive, ResetAt: shortReset, Remaining: &core.QuotaAmount{Value: 25, Unit: "percent"}}, Weekly: &core.WindowEvidence{State: core.WindowActive, ResetAt: weekReset, Remaining: &core.QuotaAmount{Value: 35, Unit: "percent"}}})
		if moving {
			shortReset, weekReset = at.Add(5*time.Hour), at.Add(7*24*time.Hour)
		}
		raw := []byte(fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`, shortReset.Unix(), weekReset.Unix()))
		read := core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: core.NextSequence(&state)}
		observation, err := ParseQuota(raw, read, at.Add(time.Second), core.ObservationQuery, state.Accounts["a"])
		if err != nil {
			t.Fatal(err)
		}
		core.ApplyObservation(&state, observation)
		account := state.Accounts["a"]
		if account.FiveHour.State != core.WindowInactive || account.Weekly.State != core.WindowInactive || len(state.History) != 1 {
			t.Fatal("real zero-used observation retained phantom countdowns")
		}
	}
}
