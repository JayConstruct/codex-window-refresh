package cpa

import (
	"fmt"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestZeroUsageFiveHourCountdownRecognizesActivation(t *testing.T) {
	at := time.Unix(1800000000, 750000000).UTC()
	for _, tc := range []struct {
		name, fields string
		minutes      int
		want         core.WindowState
	}{
		{"expired", `"reset_after_seconds":0`, 300, core.WindowExpired},
		{"last second", `"reset_after_seconds":1`, 300, core.WindowActive},
		{"one second below full", `"reset_after_seconds":17999`, 300, core.WindowActive},
		{"full placeholder", `"reset_after_seconds":18000`, 300, core.WindowUnknown},
		{"full plus tolerance", `"reset_after_seconds":18060`, 300, core.WindowUnknown},
		{"absolute shortened", `"reset_at":1800017999`, 300, core.WindowActive},
		{"absolute full with subsecond receive time", `"reset_at":1800018000`, 300, core.WindowUnknown},
		{"explicit full overrides earlier absolute sampling", `"reset_at":1800017999,"reset_after_seconds":18000`, 300, core.WindowUnknown},
		{"explicit shortened with full absolute sampling", `"reset_at":1800018000,"reset_after_seconds":17999`, 300, core.WindowActive},
		{"weekly classification unchanged", `"reset_after_seconds":17999`, 10080, core.WindowUnknown},
	} {
		for _, class := range []core.ObservationClass{core.ObservationQuery, core.ObservationExecution} {
			t.Run(tc.name+"/"+string(class), func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"window_minutes":%d,%s}}}`, tc.minutes, tc.fields))
				o, err := ParseQuota(raw, core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: 1}, at, class, core.AccountState{})
				window := o.FiveHour
				if tc.minutes == 10080 {
					window = o.Weekly
				}
				if err != nil || window == nil || window.State != tc.want || window.Remaining == nil || window.Remaining.Value != 100 {
					t.Fatalf("zero-usage countdown classified incorrectly: %+v err=%v", window, err)
				}
			})
		}
	}
}
