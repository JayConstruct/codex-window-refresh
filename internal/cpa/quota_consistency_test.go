package cpa

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestQuotaRejectsConflictingEvidence(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	read := core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: 1}
	for _, tc := range []struct{ name, fields string }{
		{"relative exceeds five hour window", `"reset_after_seconds":18061`},
		{"negative relative despite valid absolute", `"reset_at":1800000100,"reset_after_seconds":-1`},
		{"absolute expired relative positive within tolerance", `"reset_at":1799999999,"reset_after_seconds":1`},
		{"future reset contradiction", `"reset_at":1800000100,"reset_after_seconds":1000`},
		{"invalid absolute must not fall back", `"reset_at":0,"reset_after_seconds":0`},
		{"reset alias conflict", `"reset_at":1799999999,"resetAt":1800000100`},
		{"relative alias conflict", `"reset_after_seconds":0,"resetAfterSeconds":100`},
		{"legacy relative conflict", `"reset_after_seconds":0,"reset_after":100`},
		{"usage alias conflict", `"usedPercent":90,"reset_after_seconds":0`},
		{"duration alias conflict", `"limitWindowSeconds":604800,"reset_after_seconds":0`},
		{"duration units conflict", `"window_minutes":10080,"reset_after_seconds":0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":1,"limit_window_seconds":18000,%s}}}`, tc.fields))
			o, err := ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{})
			if !errors.Is(err, ErrQuotaMalformed) || o.FiveHour != nil || o.Permission != nil {
				t.Fatalf("contradictory quota produced usable evidence: %+v, %v", o, err)
			}
		})
	}
	for _, raw := range []string{
		`{"rate_limit":{"allowed":true,"limit_reached":false,"limitReached":true}}`,
		`{"rate_limit":{"allowed":true},"rateLimit":{"allowed":false}}`,
		`{"rate_limit":{"allowed":true},"rate_limits":{"allowed":false}}`,
		`{"rate_limit":{"primary_window":{"used_percent":1,"window_minutes":300,"reset_after_seconds":0},"primary":{"used_percent":1,"window_minutes":300,"reset_after_seconds":100}}}`,
	} {
		if o, err := ParseQuota([]byte(raw), read, at, core.ObservationQuery, core.AccountState{}); !errors.Is(err, ErrQuotaMalformed) || o.FiveHour != nil || o.Permission != nil {
			t.Fatalf("conflicting aliases produced usable evidence: %+v, %v", o, err)
		}
	}
}

func TestQuotaAcceptsEquivalentAliasesAndSamplingDelay(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	read := core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: 1}
	window := `{"used_percent":1,"usedPercent":1,"limit_window_seconds":18000,"limitWindowSeconds":18000,"window_minutes":300,"windowMinutes":300,"reset_at":1800000100,"resetAt":1800000100,"reset_after_seconds":120,"resetAfterSeconds":120,"reset_after":120}`
	rate := fmt.Sprintf(`{"allowed":true,"limit_reached":false,"limitReached":false,"primary_window":%s,"primary":%s}`, window, window)
	raw := []byte(fmt.Sprintf(`{"rate_limit":%s,"rate_limits":%s,"rateLimit":%s}`, rate, rate, rate))
	o, err := ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{})
	if err != nil || o.FiveHour == nil || o.FiveHour.State != core.WindowActive || !o.FiveHour.ResetAt.Equal(at.Add(100*time.Second)) {
		t.Fatalf("equivalent aliases or valid sampling delay rejected: %+v, %v", o, err)
	}
	// A countdown may remain at zero after its actual boundary has long expired.
	raw = []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"window_minutes":300,"reset_at":1799990000,"reset_after_seconds":0}}}`)
	o, err = ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{})
	if err != nil || o.FiveHour == nil || o.FiveHour.State != core.WindowExpired {
		t.Fatalf("clamped exhausted countdown rejected: %+v, %v", o, err)
	}
	// Apply the same bound to weekly windows, rather than a global ceiling.
	for _, seconds := range []int{604860, 604861} {
		raw = []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"window_minutes":10080,"reset_after_seconds":%d}}}`, seconds))
		o, err = ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{})
		if seconds == 604860 && (err != nil || o.Weekly == nil || o.Weekly.State != core.WindowActive) {
			t.Fatalf("weekly tolerance rejected: %+v %v", o, err)
		}
		if seconds == 604861 && !errors.Is(err, ErrQuotaMalformed) {
			t.Fatalf("weekly excess accepted: %+v %v", o, err)
		}
	}
}
