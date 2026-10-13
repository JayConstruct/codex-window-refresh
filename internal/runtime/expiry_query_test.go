package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestExpiryQueriesAreIndependentAndOncePerConfirmedBoundary(t *testing.T) {
	for _, kind := range []core.WindowKind{core.FiveHour, core.Weekly} {
		t.Run(string(kind), func(t *testing.T) {
			engine, host, store, clock := fixture(t, false, true)
			engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
			engine.cfg.QuotaQuery.OnFiveHourExpiry = kind == core.FiveHour
			engine.cfg.QuotaQuery.OnWeeklyExpiry = kind == core.Weekly
			account := engine.state.Accounts["a"]
			fiveReset, weekReset := account.FiveHourBaseline.StableResetAt, account.WeeklyBaseline.StableResetAt
			engine.host = controlledHost{Host: host, read: func(_ context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
				host.reads++
				permission := core.PermissionAllowed
				return core.QuotaObservation{AccountID: read.AccountID, Generation: read.Generation, ReceivedAt: clock.now, Permission: &permission, FiveHour: &core.WindowEvidence{State: core.WindowExpired, ResetAt: fiveReset}, Weekly: &core.WindowEvidence{State: core.WindowExpired, ResetAt: weekReset}}, nil
			}}
			for cycle := 0; cycle < 3; cycle++ {
				if err := engine.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				clock.now = clock.now.Add(time.Minute)
			}
			if host.reads != 1 || len(host.sends) != 0 {
				t.Fatal("expiry switch polled periodically or dispatched a model")
			}
			restarted, err := New(engine.cfg, engine.host, store, clock, fixedRandom{})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if host.reads != 1 {
				t.Fatal("restart forgot the completed expiry query")
			}
		})
	}
}

func TestExpiryQueriesDoNotUseUnknownPlaceholderOrFutureBoundary(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		engine, host, _, clock := fixture(t, false, true)
		engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
		engine.cfg.QuotaQuery.OnFiveHourExpiry, engine.cfg.QuotaQuery.OnWeeklyExpiry = true, true
		account := engine.state.Accounts["a"]
		account.FiveHourBaseline, account.WeeklyBaseline = core.WindowBaseline{}, core.WindowBaseline{}
		account.FiveHour = core.WindowEvidence{State: core.WindowUnknown, ResetAt: clock.now.Add(5 * time.Hour), Remaining: &core.QuotaAmount{Value: 100, Unit: "percent"}}
		if !unknown {
			account.FiveHourBaseline.StableResetAt = clock.now.Add(time.Hour)
			account.WeeklyBaseline.StableResetAt = clock.now.Add(24 * time.Hour)
		}
		engine.state.Accounts["a"] = account
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.reads != 0 || len(host.sends) != 0 {
			t.Fatal("expiry query treated a placeholder or future reset as expired")
		}
	}
}

func TestFailedAndPartialExpiryQueriesRetryWithExistingThrottle(t *testing.T) {
	for _, partial := range []bool{false, true} {
		engine, host, _, clock := fixture(t, false, false)
		engine.cfg.Enabled, engine.cfg.QuotaQuery.Automatic = false, false
		engine.cfg.QuotaQuery.OnFiveHourExpiry = true
		reads := 0
		engine.host = controlledHost{Host: host, read: func(ctx context.Context, read core.QuotaRead) (core.QuotaObservation, error) {
			reads++
			if reads == 1 {
				if !partial {
					return core.QuotaObservation{}, errors.New("private network failure")
				}
				permission := core.PermissionAllowed
				return core.QuotaObservation{AccountID: read.AccountID, Generation: read.Generation, ReceivedAt: clock.now, Permission: &permission}, nil
			}
			return host.ObserveQuota(ctx, read)
		}}
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !engine.state.Accounts["a"].FiveHourExpiryQueried.IsZero() {
			t.Fatal("failure or missing window marked the cycle as queried")
		}
		clock.now = clock.now.Add(29 * time.Second)
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if reads != 1 {
			t.Fatal("expiry query retried each scheduler tick")
		}
		clock.now = clock.now.Add(time.Second)
		if err := engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if reads != 2 || engine.state.Accounts["a"].FiveHourExpiryQueried.IsZero() || len(host.sends) != 0 {
			t.Fatal("expiry query did not retry safely after its throttle")
		}
	}
}
