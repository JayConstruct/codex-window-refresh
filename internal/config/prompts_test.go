package config

import (
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func TestRandomDefaultsDoNotMigrateOldMessagesOrEnableQueries(t *testing.T) {
	settings := valid(t, `{"execution":{"prompt":"old default"},"reset_rules":{"five_hour":{"prompt":"old five"},"weekly":{"prompt":"old week"}}}`, "json")
	if settings.Execution.PromptMode != "random" || settings.QuotaQuery.FiveHourRecentUsage.Enabled || settings.QuotaQuery.FiveHourRecentUsage.Lookback.Value() != 30*time.Minute {
		t.Fatal("new defaults enabled automatic work or retained fixed mode")
	}
	for _, reason := range []core.Reason{core.ReasonManual, core.ReasonFiveHour, core.ReasonWeekly} {
		for _, prompt := range settings.WakePromptPool(reason) {
			if strings.HasPrefix(prompt, "old") {
				t.Fatal("legacy messages migrated into random pool")
			}
		}
	}
	settings.ResetRules.FiveHour.PromptPool = []string{"dedicated"}
	if settings.WakePromptPool(core.ReasonDaily)[0] != "dedicated" || settings.WakePromptPool(core.ReasonWeekly)[0] != "Reply OK." {
		t.Fatal("dedicated pool inheritance changed")
	}
	pool := settings.WakePromptPool(core.ReasonDaily)
	pool[0] = "mutated"
	if settings.ResetRules.FiveHour.PromptPool[0] != "dedicated" {
		t.Fatal("pool accessor leaked mutable configuration")
	}
}

func TestPromptPoliciesRejectMalformedPoolsAndUsageDurations(t *testing.T) {
	for _, fragment := range []string{
		`"execution":{"prompt_mode":"other"}`, `"execution":{"prompt_pool":[]}`, `"execution":{"prompt_pool":null}`,
		`"execution":{"prompt_pool":["same"," same "]}`, `"execution":{"prompt_pool":[""]}`, `"execution":{"prompt_pool":["line\nbreak"]}`,
		`"execution":{"prompt_pool":["` + strings.Repeat("a", 257) + `"]}`,
		`"quota_query":{"five_hour_recent_usage":{"lookback":"0s"}}`, `"quota_query":{"five_hour_recent_usage":{"lookback":"8d"}}`,
		`"quota_query":{"five_hour_recent_usage":{"enabled":"true"}}`,
	} {
		if _, err := Parse([]byte(`{`+fragment+`}`), "json"); err == nil {
			t.Fatalf("invalid policy accepted: %s", fragment)
		}
	}
	valid(t, `{"execution":{"prompt_mode":"fixed","prompt_pool":[]}}`, "json")
}
