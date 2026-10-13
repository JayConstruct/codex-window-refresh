package core

import (
	"testing"
	"time"
)

func TestManualAdmissionBypassesPolicyButNotSafety(t *testing.T) {
	input := ordinaryInput()
	input.Task.Intents[0].Reason = ReasonManual
	input.WithinAllowedHours = false
	input.OrdinarySlotsAvailable = 0
	input.FiveHour = WindowEvidence{Kind: FiveHour, State: WindowUnknown, ResetAt: input.Now.Add(5 * time.Hour), ConfirmedAt: input.Now, Source: "cpa_wham_usage", Remaining: &QuotaAmount{Value: 100, Unit: "percent"}}
	if !DecideAdmission(input).Allowed {
		t.Fatal("explicit wake was blocked by automatic policy or full unstarted window")
	}
	input.Task.Intents[0].Reason = ReasonFiveHour
	if DecideAdmission(input).Allowed {
		t.Fatal("full quota was treated as automatic activation evidence")
	}
}
