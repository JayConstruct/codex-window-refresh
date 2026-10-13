package config

import (
	"strings"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestGranularSettingsDefaultsAndCompatibility(t *testing.T) {
	settings := valid(t, `{"execution":{"model":"default"}}`, "json")
	if settings.QuotaQuery.OnFiveHourExpiry || settings.QuotaQuery.OnWeeklyExpiry || settings.Planning.Enabled || settings.Planning.HorizonDays != 7 || !settings.OrdinaryRules.DailyEnabled {
		t.Fatal("new automatic features enabled themselves or disabled legacy daily rules")
	}
	for _, reason := range []core.Reason{core.ReasonFiveHour, core.ReasonWeekly, core.ReasonDaily, core.ReasonManual} {
		model, prompt := settings.WakeContent(reason)
		if model != "default" || prompt != "Reply OK." {
			t.Fatal("legacy execution content was not inherited")
		}
	}
	settings = valid(t, `{"enabled":true,"reset_rules":{"five_hour":{"enabled":false},"weekly":{"model":"weekly-model","prompt":"weekly-message"}}}`, "json")
	model, prompt := settings.WakeContent(core.ReasonWeekly)
	if model != "weekly-model" || prompt != "weekly-message" {
		t.Fatal("dedicated weekly model did not satisfy independent configuration")
	}
}

func TestGranularSettingsRejectInvalidFields(t *testing.T) {
	for _, fragment := range []string{
		`"quota_query":{"on_five_hour_expiry":"true"}`, `"quota_query":{"on_weekly_expiry":null}`,
		`"planning":{"enabled":"true"}`, `"planning":{"horizon_days":0}`, `"planning":{"horizon_days":31}`,
		`"planning":{"horizon_days":1.5}`, `"planning":{"unknown":true}`, `"ordinary_rules":{"daily_enabled":"false"}`,
		`"reset_rules":{"five_hour":{"model":" "}}`, `"reset_rules":{"weekly":{"prompt":" "}}`,
		`"reset_rules":{"five_hour":{"model":"REPLACE_WITH_MODEL"}}`,
		`"reset_rules":{"weekly":{"prompt":"` + strings.Repeat("x", 4097) + `"}}`,
	} {
		if _, err := Parse([]byte(`{`+fragment+`}`), "json"); err == nil {
			t.Fatalf("invalid granular setting accepted: %s", fragment)
		}
	}
}
