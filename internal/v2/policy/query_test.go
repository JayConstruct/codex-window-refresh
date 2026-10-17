package policy

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultQueryIsValidAndAutomaticQueriesAreOptIn(t *testing.T) {
	config := DefaultQuery()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if config.Automatic || config.FiveHour || config.Weekly || config.Delay != (Delay{Min: 5, Max: 60}) {
		t.Fatalf("unsafe defaults: %+v", config)
	}
	for _, trigger := range []string{"periodic", "five_hour_expiry", "weekly_expiry", "initial_sync", "manual_single", "manual_batch", "model"} {
		if config.Enabled(trigger) {
			t.Errorf("default enabled %q", trigger)
		}
	}
	for _, trigger := range []string{"periodic", "five_hour_expiry", "weekly_expiry"} {
		t.Run(trigger, func(t *testing.T) {
			config := DefaultQuery()
			config.Automatic = trigger == "periodic"
			config.FiveHour = trigger == "five_hour_expiry"
			config.Weekly = trigger == "weekly_expiry"
			if !config.Enabled(trigger) || !config.Enabled("initial_sync") || config.Enabled("model") {
				t.Fatalf("incorrect trigger mapping: %+v", config)
			}
		})
	}
}

func TestQueryValidationRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*QueryConfig)
	}{
		{"empty timezone", func(config *QueryConfig) { config.Timezone = "" }},
		{"process timezone", func(config *QueryConfig) { config.Timezone = "Local" }},
		{"unknown timezone", func(config *QueryConfig) { config.Timezone = "Not/A_Timezone" }},
		{"short interval", func(config *QueryConfig) { config.Interval = 29 }},
		{"long interval", func(config *QueryConfig) { config.Interval = 3601 }},
		{"negative delay", func(config *QueryConfig) { config.Delay.Min = -1 }},
		{"reversed delay", func(config *QueryConfig) { config.Delay = Delay{Min: 61, Max: 60} }},
		{"excessive delay", func(config *QueryConfig) { config.Delay.Max = 86401 }},
		{"malformed hours", func(config *QueryConfig) { config.ExcludedHours = []string{"night"} }},
		{"invalid hour", func(config *QueryConfig) { config.ExcludedHours = []string{"25:00-06:00"} }},
		{"invalid minute", func(config *QueryConfig) { config.ExcludedHours = []string{"22:60-06:00"} }},
		{"empty span", func(config *QueryConfig) { config.ExcludedHours = []string{"06:00-06:00"} }},
		{"duplicate spans", func(config *QueryConfig) { config.ExcludedHours = []string{"09:00-10:00", "09:00-10:00"} }},
		{"overnight overlap", func(config *QueryConfig) { config.ExcludedHours = []string{"22:00-06:00", "05:59-07:00"} }},
		{"too many spans", func(config *QueryConfig) { config.ExcludedHours = make([]string, 25) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultQuery()
			test.change(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid; config=%+v", err, config)
			}
		})
	}
	for _, config := range []QueryConfig{
		{Timezone: "UTC", Interval: 30, Delay: Delay{Min: 0, Max: 0}},
		{Timezone: "Asia/Shanghai", Interval: 3600, Delay: Delay{Min: 86400, Max: 86400}, ExcludedHours: []string{"22:00-06:00", "06:00-07:00"}},
		{Timezone: "UTC", Interval: 300, ExcludedHours: []string{"00:00-24:00"}},
	} {
		if err := config.Validate(); err != nil {
			t.Errorf("valid boundary configuration rejected: %+v: %v", config, err)
		}
	}
}

func TestQueryAllowedUsesTimezoneAndHalfOpenOvernightSpans(t *testing.T) {
	tests := []struct {
		name     string
		timezone string
		hours    []string
		at       string
		allowed  bool
	}{
		{"before overnight start", "Asia/Shanghai", []string{"22:00-06:00"}, "2030-01-01T13:59:59Z", true},
		{"overnight start", "Asia/Shanghai", []string{"22:00-06:00"}, "2030-01-01T14:00:00Z", false},
		{"local midnight", "Asia/Shanghai", []string{"22:00-06:00"}, "2030-01-01T16:00:00Z", false},
		{"before overnight end", "Asia/Shanghai", []string{"22:00-06:00"}, "2030-01-01T21:59:59Z", false},
		{"overnight end", "Asia/Shanghai", []string{"22:00-06:00"}, "2030-01-01T22:00:00Z", true},
		{"UTC is not Shanghai", "UTC", []string{"22:00-06:00"}, "2030-01-01T14:00:00Z", true},
		{"daytime start", "UTC", []string{"09:00-10:00"}, "2030-01-01T09:00:00Z", false},
		{"daytime end", "UTC", []string{"09:00-10:00"}, "2030-01-01T10:00:00Z", true},
		{"all day", "UTC", []string{"00:00-24:00"}, "2030-01-01T23:59:59Z", false},
		{"DST winter", "America/New_York", []string{"09:00-10:00"}, "2030-01-01T14:00:00Z", false},
		{"DST summer", "America/New_York", []string{"09:00-10:00"}, "2030-07-01T13:00:00Z", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultQuery()
			config.Timezone, config.ExcludedHours = test.timezone, test.hours
			if err := config.Validate(); err != nil {
				t.Fatal(err)
			}
			at, err := time.Parse(time.RFC3339, test.at)
			if err != nil {
				t.Fatal(err)
			}
			if allowed := config.Allowed(at); allowed != test.allowed {
				t.Fatalf("Allowed(%s) = %v, want %v", at, allowed, test.allowed)
			}
		})
	}
	config := DefaultQuery()
	config.Timezone = "Not/A_Timezone"
	if config.Allowed(time.Now()) {
		t.Fatal("invalid timezone allowed dispatch")
	}
	config = DefaultQuery()
	config.ExcludedHours = []string{"invalid"}
	if config.Allowed(time.Now()) {
		t.Fatal("malformed exclusion allowed dispatch")
	}
}
