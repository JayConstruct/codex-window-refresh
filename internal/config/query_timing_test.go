package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWindowQuerySettingsPreserveLegacyAndRoundTrip(t *testing.T) {
	legacy := valid(t, `{"quota_query":{"automatic":true,"interval":"2m"}}`, "json")
	for _, weekly := range []bool{false, true} {
		window := legacy.QuotaQuery.Window(weekly)
		if !window.PeriodicEnabled || window.Interval.Value() != 2*time.Minute || len(window.Expiry.ExcludedHours) != 0 || window.Periodic.RandomDelay.Max != 0 {
			t.Fatal("legacy periodic behavior changed")
		}
	}
	settings := valid(t, `{"quota_query":{"five_hour":{"periodic_enabled":false,"expiry":{"excluded_hours":["22:00-08:00"],"random_delay":{"min":"1m","max":"5m"}}},"weekly":{"interval":"10m","periodic":{"excluded_hours":["00:00-24:00"]}}}}`, "json")
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	restored := valid(t, string(data), "json")
	if restored.QuotaQuery.FiveHour.PeriodicEnabled || !restored.QuotaQuery.Weekly.PeriodicEnabled || restored.QuotaQuery.FiveHour.Expiry.RandomDelay.Max.Value() != 5*time.Minute || restored.QuotaQuery.Weekly.Interval.Value() != 10*time.Minute {
		t.Fatal("independent query settings did not survive serialization")
	}
	if settings.QuotaQuery.Automatic || settings.QuotaQuery.OnFiveHourExpiry || settings.QuotaQuery.OnWeeklyExpiry {
		t.Fatal("timing settings enabled automatic requests")
	}
}

func TestWindowQuerySettingsRejectInvalidPolicies(t *testing.T) {
	for _, fragment := range []string{
		`"five_hour":{"interval":"29s"}`, `"weekly":{"periodic_enabled":"true"}`,
		`"five_hour":{"expiry":{"excluded_hours":["09:00-10:00","09:30-11:00"]}}`,
		`"weekly":{"periodic":{"excluded_hours":["25:00-26:00"]}}`,
		`"weekly":{"periodic":{"excluded_hours":null}}`,
		`"five_hour":{"expiry":{"random_delay":{"min":"2m","max":"1m"}}}`,
		`"weekly":{"periodic":{"random_delay":{"max":"25h"}}}`,
		`"interval":"2m","five_hour":{"interval":"1m"}`, `"five_hour":{"expiry":{"unknown":true}}`,
	} {
		if _, err := Parse([]byte(`{"quota_query":{`+fragment+`}}`), "json"); err == nil {
			t.Fatalf("invalid query policy accepted: %s", fragment)
		}
	}
}
