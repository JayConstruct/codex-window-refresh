package core

import "time"

func bothFull(short, week *WindowEvidence) bool {
	full := func(window *WindowEvidence) bool {
		return window != nil && window.Remaining != nil && window.Remaining.Unit == "percent" && window.Remaining.Value == 100
	}
	return full(short) && full(week)
}

func consumedFutureWindow(window WindowEvidence, baseline WindowBaseline, at time.Time) bool {
	consumed := !baseline.LastConsumedAt.IsZero() && !baseline.LastConsumedAt.After(at)
	consumed = consumed || (window.State == WindowActive && window.Remaining != nil && window.Remaining.Unit == "percent" && window.Remaining.Value < 100 && window.Remaining.Value >= 0 && baseline.StableResetAt.Equal(window.ResetAt))
	return consumed && baseline.StableResetAt.After(at)
}
