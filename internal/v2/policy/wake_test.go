package policy

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestWakeDefaultsOffAndNormalizeCopiesHours(t *testing.T) {
	config := DefaultWake()
	if err := config.Validate(); err != nil || config.Enabled || config.FiveHour || config.Weekly || config.DailyEnabled || config.Timezone != "Asia/Shanghai" || config.AccountsPerUser != 1 || config.MaxAccounts != 10000 || config.DailyTarget != 1 {
		t.Fatalf("defaults=%+v err=%v", config, err)
	}
	for _, reason := range []string{"five_hour_reset", "weekly_reset", "daily", "unknown"} {
		if config.RuleEnabled(reason) {
			t.Fatalf("default enabled %q", reason)
		}
	}
	if !config.RuleEnabled("manual") {
		t.Fatal("manual wake depends on automation switch")
	}
	normalized := config.Normalize()
	if !reflect.DeepEqual(config, normalized) {
		t.Fatal("normalization changed values")
	}
	normalized.AllowedHours[0] = "00:00-24:00"
	if config.AllowedHours[0] != "08:00-23:00" {
		t.Fatal("normalization retained slice alias")
	}
	config.AllowedHours = nil
	if config.Normalize().AllowedHours == nil {
		t.Fatal("nil hours were not normalized")
	}
}

func TestWakeConfigValidationBounds(t *testing.T) {
	tests := []struct {
		name   string
		change func(*WakeConfig)
	}{
		{"empty timezone", func(config *WakeConfig) { config.Timezone = "" }},
		{"local timezone", func(config *WakeConfig) { config.Timezone = "Local" }},
		{"unknown timezone", func(config *WakeConfig) { config.Timezone = "Invalid/Timezone" }},
		{"negative delay", func(config *WakeConfig) { config.Delay.Min = -1 }},
		{"reversed delay", func(config *WakeConfig) { config.Delay = Delay{Min: 10, Max: 9} }},
		{"excess delay", func(config *WakeConfig) { config.Delay.Max = 86401 }},
		{"malformed hours", func(config *WakeConfig) { config.AllowedHours = []string{"8:00-09:00"} }},
		{"overlapping hours", func(config *WakeConfig) { config.AllowedHours = []string{"08:00-10:00", "09:00-11:00"} }},
		{"zero-width hours", func(config *WakeConfig) { config.AllowedHours = []string{"08:00-08:00"} }},
		{"24 hour start", func(config *WakeConfig) { config.AllowedHours = []string{"24:00-01:00"} }},
		{"invalid daily window", func(config *WakeConfig) { config.DailyWindow = "9:00-10:00" }},
		{"overnight daily window", func(config *WakeConfig) { config.DailyWindow = "23:00-01:00" }},
		{"zero daily window", func(config *WakeConfig) { config.DailyWindow = "09:00-09:00" }},
		{"invalid midnight", func(config *WakeConfig) { config.DailyWindow = "23:00-24:01" }},
		{"ratio negative", func(config *WakeConfig) { config.AccountsPerUser = -0.01 }},
		{"ratio excessive", func(config *WakeConfig) { config.AccountsPerUser = 100.01 }},
		{"ratio NaN", func(config *WakeConfig) { config.AccountsPerUser = math.NaN() }},
		{"ratio positive infinity", func(config *WakeConfig) { config.AccountsPerUser = math.Inf(1) }},
		{"ratio negative infinity", func(config *WakeConfig) { config.AccountsPerUser = math.Inf(-1) }},
		{"zero max accounts", func(config *WakeConfig) { config.MaxAccounts = 0 }},
		{"excess max accounts", func(config *WakeConfig) { config.MaxAccounts = 10001 }},
		{"negative daily target", func(config *WakeConfig) { config.DailyTarget = -1 }},
		{"daily exceeds cap", func(config *WakeConfig) { config.MaxAccounts, config.DailyTarget = 2, 3 }},
		{"enabled without rules", func(config *WakeConfig) { config.Enabled = true }},
		{"too many allowed spans", func(config *WakeConfig) {
			config.AllowedHours = nil
			for hour := 0; hour < 25; hour++ {
				config.AllowedHours = append(config.AllowedHours, fmt.Sprintf("%02d:00-%02d:30", hour%24, hour%24))
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultWake()
			test.change(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalidWake) {
				t.Fatalf("invalid config=%+v err=%v", config, err)
			}
		})
	}
	for _, ratio := range []float64{0, 0.25, 1, 100} {
		config := DefaultWake()
		config.AccountsPerUser, config.Delay, config.DailyTarget = ratio, Delay{Min: 0, Max: 86400}, 0
		config.AllowedHours, config.DailyWindow = []string{"00:00-24:00"}, "23:59-24:00"
		if err := config.Validate(); err != nil {
			t.Fatalf("valid boundary=%+v err=%v", config, err)
		}
	}
}

func TestWakeAllowedHoursUseLocalTimeAndHalfOpenBounds(t *testing.T) {
	config := DefaultWake()
	for _, test := range []struct {
		stamp string
		want  bool
	}{
		{"2026-10-17T23:59:59Z", false},
		{"2026-10-18T00:00:00Z", true},
		{"2026-10-18T14:59:59Z", true},
		{"2026-10-18T15:00:00Z", false},
	} {
		at, err := time.Parse(time.RFC3339, test.stamp)
		if err != nil {
			t.Fatal(err)
		}
		if got := config.InAllowedHours(at); got != test.want {
			t.Fatalf("allowed(%s)=%v want %v", test.stamp, got, test.want)
		}
	}
	config.Timezone = "UTC"
	config.AllowedHours = []string{"23:00-01:00"}
	if config.Validate() != nil {
		t.Fatal("overnight allowed hours should remain supported")
	}
	for hour, want := range map[int]bool{0: true, 1: false, 22: false, 23: true} {
		if config.InAllowedHours(time.Date(2026, 10, 18, hour, 0, 0, 0, time.UTC)) != want {
			t.Fatalf("overnight allowed mismatch at %d", hour)
		}
	}
	config.AllowedHours = nil
	if !config.InAllowedHours(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("empty hours should allow all times")
	}
}

func TestWakeDailyBoundsLocalDateMidnightAndCatchUp(t *testing.T) {
	config := DefaultWake()
	for _, test := range []struct {
		stamp string
		ready bool
	}{
		{"2026-10-18T00:59:59Z", false},
		{"2026-10-18T01:00:00Z", true},
		{"2026-10-18T01:10:00Z", true},
		{"2026-10-18T14:00:00Z", true},
	} {
		at, _ := time.Parse(time.RFC3339, test.stamp)
		day, start, end, ready := config.DailyBounds(at)
		if day != "2026-10-18" || start.UTC().Format(time.RFC3339) != "2026-10-18T01:00:00Z" || end.UTC().Format(time.RFC3339) != "2026-10-18T01:10:00Z" || ready != test.ready {
			t.Fatalf("bounds(%s)=%s %s %s %v", test.stamp, day, start, end, ready)
		}
	}
	config.DailyWindow = "23:00-24:00"
	at := time.Date(2026, 10, 18, 15, 0, 0, 0, time.UTC)
	day, start, end, ready := config.DailyBounds(at)
	if day != "2026-10-18" || !ready || !start.Equal(at) || !end.Equal(at.Add(time.Hour)) || end.In(start.Location()).Format("2006-01-02 15:04") != "2026-10-19 00:00" {
		t.Fatalf("midnight bounds=%s %s %s %v", day, start, end, ready)
	}
	day, _, _, ready = config.DailyBounds(at.Add(time.Hour))
	if day != "2026-10-19" || ready {
		t.Fatalf("daily date did not roll over: %s %v", day, ready)
	}
	for _, invalid := range []WakeConfig{
		{Timezone: "Invalid/Timezone", DailyWindow: "09:00-10:00"},
		{Timezone: "UTC", DailyWindow: "23:00-01:00"},
		{Timezone: "UTC", DailyWindow: "broken"},
	} {
		day, start, end, ready := invalid.DailyBounds(at)
		if day != "" || !start.IsZero() || !end.IsZero() || ready {
			t.Fatalf("invalid bounds accepted: %s %s %s %v", day, start, end, ready)
		}
	}
}

func TestWakeRuleFlagsAndModelOverridesAreIndependent(t *testing.T) {
	config := DefaultWake()
	config.FiveHour, config.Weekly, config.DailyEnabled = true, true, true
	for _, reason := range []string{"five_hour_reset", "weekly_reset", "daily"} {
		if config.RuleEnabled(reason) {
			t.Fatal("disabled master switch ignored")
		}
	}
	config.Enabled = true
	for _, reason := range []string{"five_hour_reset", "weekly_reset", "daily", "manual"} {
		if !config.RuleEnabled(reason) {
			t.Fatalf("enabled rule %q ignored", reason)
		}
	}
	config.FiveHour, config.Weekly, config.DailyTarget = false, false, 0
	for _, reason := range []string{"five_hour_reset", "weekly_reset", "daily", "unknown"} {
		if config.RuleEnabled(reason) {
			t.Fatalf("disabled rule %q enabled", reason)
		}
	}
	models := DefaultModel()
	models.DefaultModel, models.DefaultPrompt = "default-model", "default-prompt"
	models.FiveHourModel, models.WeeklyPrompt = "five-model", "weekly-prompt"
	for _, test := range []struct{ reason, model, prompt string }{
		{"manual", "default-model", "default-prompt"},
		{"daily", "default-model", "default-prompt"},
		{"five_hour_reset", "five-model", "default-prompt"},
		{"weekly_reset", "default-model", "weekly-prompt"},
	} {
		model, prompt := WakeModel(models, test.reason)
		if model != test.model || prompt != test.prompt {
			t.Fatalf("model(%s)=%q %q", test.reason, model, prompt)
		}
	}
	models.FiveHourPrompt, models.WeeklyModel = " \n\t", "weekly-model"
	if model, prompt := WakeModel(models, "five_hour_reset"); model != "five-model" || prompt != "default-prompt" {
		t.Fatal("blank override did not inherit")
	}
	if model, prompt := WakeModel(models, "weekly_reset"); model != "weekly-model" || prompt != "weekly-prompt" {
		t.Fatal("weekly overrides not applied")
	}
}
