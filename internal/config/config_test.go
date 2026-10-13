package config

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func valid(t *testing.T, data, format string) Config {
	t.Helper()
	c, err := Parse([]byte(data), format)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestParseDefaultsAndForceFreshObservation(t *testing.T) {
	c := valid(t, `{"execution":{"model":"supported","quota_max_age_before_send":"0s"},"daily_rules":[{"id":"morning","random_time_range":"09:00-09:10"}]}`, "json")
	if c.Enabled || c.QuotaQuery.Automatic || !c.QuotaQuery.Manual || c.QuotaQuery.Interval.Value() != 30*time.Second {
		t.Fatal("automatic work must require explicit opt-in while manual queries stay allowed")
	}
	if c.Activity.IdentitySource != "host_caller_scope" || c.Activity.Lookback.Value() != 30*time.Minute || c.Execution.VerificationCapacityHold.Value() != 15*time.Minute {
		t.Fatalf("defaults lost: %+v", c)
	}
	if c.Execution.QuotaReadTimeout.Value() != 15*time.Second || c.Execution.RequestTimeout.Value() != time.Minute {
		t.Fatalf("existing configuration did not inherit timeout defaults: %+v", c.Execution)
	}
	if !c.DailyRules[0].Enabled || !c.DailyRules[0].CatchUp || c.DailyRules[0].MinimumReadyAccounts != 1 {
		t.Fatalf("daily defaults lost: %+v", c.DailyRules[0])
	}
	r := c.Runtime()
	if !r.ForceObserveBeforeDispatch || r.EvidenceMaxAge != 30*time.Second {
		t.Fatalf("zero freshness semantics lost: %+v", r)
	}
	c.Execution.QuotaMaxAgeBeforeSend = Duration(5 * time.Second)
	if c.Runtime().ForceObserveBeforeDispatch || c.Runtime().EvidenceMaxAge != 5*time.Second {
		t.Fatal("positive freshness mapping changed")
	}
}
func TestQuerySettingsParseIndependentlyAndRejectInvalidTypes(t *testing.T) {
	c := valid(t, `{"enabled":false,"execution":{"model":"supported"},"quota_query":{"automatic":true,"manual":false,"interval":"2m"}}`, "json")
	if c.Enabled || !c.QuotaQuery.Automatic || !c.QuotaQuery.Manual || c.QuotaQuery.Interval.Value() != 2*time.Minute {
		t.Fatal("independent query config lost")
	}
	for _, settings := range []string{`{"automatic":"false"}`, `{"manual":null}`, `{"interval":"29s"}`, `{"interval":"3601s"}`, `{"interval":30}`, `{"automatic":false,"automatic":true}`, `{"model":"unsupported"}`} {
		if _, err := Parse([]byte(`{"execution":{"model":"supported"},"quota_query":`+settings+`}`), "json"); err == nil {
			t.Fatalf("invalid query config accepted: %s", settings)
		}
	}
}
func TestQuotaOnlyConfigAllowsDeferredModelButCannotEnableWakeups(t *testing.T) {
	settings := valid(t, `{"enabled":false,"execution":{"model":""}}`, "json")
	if settings.QuotaQuery.Automatic || !settings.QuotaQuery.Manual {
		t.Fatal("quota-only configuration did not preserve manual-only defaults")
	}
	settings.Enabled = true
	if err := settings.Validate(); err == nil {
		t.Fatal("wakeups enabled without a model")
	}
}
func TestAutomaticFeaturesRequireExplicitOptIn(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		data := `{}`
		if format == "yaml" {
			data = "quota_query:\n  manual: false\n"
		}
		settings := valid(t, data, format)
		if settings.Enabled || settings.QuotaQuery.Automatic || !settings.QuotaQuery.Manual {
			t.Fatalf("%s did not keep automatic features off and manual queries available", format)
		}
	}
	settings := valid(t, `{"enabled":true,"execution":{"model":"supported"},"quota_query":{"automatic":true,"manual":false}}`, "json")
	if !settings.Enabled || !settings.QuotaQuery.Automatic || !settings.QuotaQuery.Manual {
		t.Fatal("explicit automatic opt-in or always-allowed manual query lost")
	}
}

func TestYAMLStrongScalarTypesAndDuplicateRejection(t *testing.T) {
	c := valid(t, "execution:\n  model: supported\nactivity:\n  lookback: 1d\n  accounts_per_user: 0.5\n", "yaml")
	if c.Activity.Lookback.Value() != 24*time.Hour || c.Activity.AccountsPerUser != 0.5 {
		t.Fatal("YAML decode mismatch")
	}
	cases := []struct{ format, data string }{
		{"yaml", "execution: {model: supported}\nactivity: {accounts_per_user: .nan}"},
		{"yaml", "execution: {model: supported}\nactivity: {accounts_per_user: .inf}"},
		{"yaml", "execution: {model: supported}\nactivity: {accounts_per_user: '0.5'}"},
		{"yaml", "execution: {model: supported}\nenabled: 'true'"},
		{"yaml", "execution: {model: supported}\nenabled: yes"},
		{"yaml", "execution: {model: supported}\nactivity: {max_accounts: 1.0}"},
		{"yaml", "execution: {model: supported, model: other}"},
		{"yaml", "execution: {model: supported}\nactivity: null"},
		{"yaml", "execution: {model: supported}\n---\nenabled: false"},
		{"json", `{"execution":{"model":"supported","model":"other"}}`},
		{"json", `{"execution":{"model":"supported","unknown":true}}`},
		{"json", `{"execution":{"model":"supported"},"enabled":null}`},
		{"json", `{"execution":{"model":"supported"}} {}`},
		{"json", `[]`},
		{"yaml", "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.format+tc.data, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data), tc.format); err == nil {
				t.Fatal("unsafe or malformed configuration accepted")
			}
		})
	}
}
func TestInvalidPolicyFields(t *testing.T) {
	cases := []struct{ fragment, code string }{
		{`"timezone":"Bad/Zone"`, "INVALID_TIMEZONE"},
		{`"reset_rules":{"weekly":{"bypass_activity_limit":false}}`, "WEEKLY_EXEMPTION_REQUIRED"},
		{`"reset_rules":{"weekly":{"bypass_allowed_hours":false}}`, "WEEKLY_EXEMPTION_REQUIRED"},
		{`"activity":{"accounts_per_user":101}`, "OUT_OF_RANGE"},
		{`"execution":{"model":"supported","catch_up":false}`, "CATCH_UP_REQUIRED"},
		{`"execution":{"model":"supported","verify_delays":["5s","5s"]}`, "VERIFY_DELAYS_NOT_INCREASING"},
		{`"execution":{"model":"supported","verify_delays":["20m"]}`, "OUT_OF_RANGE"},
		{`"execution":{"model":"supported","quota_read_timeout":"0s"}`, "INVALID_DURATION"},
		{`"execution":{"model":"supported","quota_read_timeout":"301s"}`, "INVALID_DURATION"},
		{`"execution":{"model":"supported","request_timeout":"0s"}`, "INVALID_DURATION"},
		{`"execution":{"model":"supported","request_timeout":"1801s"}`, "INVALID_DURATION"},
		{`"daily_rules":[{"id":"x","random_time_range":"22:00-01:00"}]`, "DAILY_RANGE_CROSSES_MIDNIGHT"},
		{`"daily_rules":[{"id":"x","random_time_range":"09:00-09:00"},{"id":"x","random_time_range":"09:00-09:10"}]`, "DUPLICATE_RULE_ID"},
	}
	for _, tc := range cases {
		data := `{"execution":{"model":"supported"},` + tc.fragment + `}`
		// Avoid intentionally duplicated execution key: each fragment is standalone.
		if strings.HasPrefix(tc.fragment, `"execution"`) {
			data = `{` + tc.fragment + `}`
		}
		_, err := Parse([]byte(data), "json")
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("%s: expected structured validation error, got %v", tc.fragment, err)
		}
		found := false
		for _, f := range ve.Fields {
			found = found || f.Code == tc.code
		}
		if !found {
			t.Fatalf("%s: missing %s in %+v", tc.fragment, tc.code, ve.Fields)
		}
	}
	c := Defaults()
	c.Execution.Model = "supported"
	c.Activity.AccountsPerUser = math.NaN()
	if err := c.Validate(); err == nil {
		t.Fatal("programmatic NaN accepted")
	}
}
func TestDurationGrammarOverflowAndRoundTrip(t *testing.T) {
	for _, value := range []string{"1.5h", "-1s", "1h30m", "1ms", "9223372036854775807d", "18446744073709551616s", "1"} {
		_, err := Parse([]byte(`{"execution":{"model":"supported","min_dispatch_spacing":"`+value+`"}}`), "json")
		if err == nil {
			t.Fatalf("invalid duration accepted: %s", value)
		}
	}
	c := valid(t, `{"execution":{"model":"supported"}}`, "json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data, "json")
	if err != nil {
		t.Fatal(err)
	}
	if again.Execution.VerifyDelays[2] != Duration(2*time.Minute) {
		t.Fatal("duration round trip mismatch")
	}
}
func TestAllowedHoursCrossMidnightAndHalfOpen(t *testing.T) {
	c := valid(t, `{"execution":{"model":"supported"},"ordinary_rules":{"allowed_hours":["22:00-02:00"]}}`, "json")
	for _, tc := range []struct {
		clock   string
		allowed bool
	}{{"21:59:59", false}, {"22:00:00", true}, {"01:59:59", true}, {"02:00:00", false}} {
		at, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-11 "+tc.clock, c.Location())
		if err != nil {
			t.Fatal(err)
		}
		if got := c.WithinAllowedHours(at, "account"); got != tc.allowed {
			t.Fatalf("%s: %v want %v", tc.clock, got, tc.allowed)
		}
	}
	for _, v := range [][]string{{"22:00-02:00", "01:00-03:00"}, {"08:00-08:00"}, {"24:00-02:00"}} {
		if _, err := AllowedRanges(v); err == nil {
			t.Fatalf("invalid range accepted: %v", v)
		}
	}
	ranges, err := AllowedRanges([]string{"08:00-12:00", "12:00-23:00"})
	if err != nil || len(ranges) != 1 || ranges[0] != (MinuteRange{480, 1380}) {
		t.Fatalf("adjacent merge: %+v %v", ranges, err)
	}
	if ranges, err := AllowedRanges([]string{}); err != nil || len(ranges) != 0 {
		t.Fatal("empty allowed hours should disable normal hours")
	}
}
func TestAccountEligibilitySeparateFromGlobalEventSwitch(t *testing.T) {
	c := valid(t, `{"execution":{"model":"supported"},"reset_rules":{"five_hour":{"enabled":false},"weekly":{"enabled":false}},"account_overrides":{"a":{"five_hour_enabled":true,"weekly_enabled":true},"b":{"five_hour_enabled":false,"weekly_enabled":true,"allowed_hours":[]}}}`, "json")
	a, b := c.Account("a"), c.Account("b")
	if !a.OrdinaryParticipant || a.FiveHourEnabled || a.WeeklyEnabled {
		t.Fatalf("account bypassed global event switch: %+v", a)
	}
	if b.OrdinaryParticipant || b.WeeklyEnabled || len(b.AllowedHours) != 0 {
		t.Fatalf("disabled participant: %+v", b)
	}
	c.ResetRules.Weekly.Enabled = true
	if !c.Account("b").WeeklyEnabled {
		t.Fatal("ordinary participation incorrectly controls weekly")
	}
	inherited := c.Account(core.AccountID("missing"))
	inherited.AllowedHours[0] = "00:00-24:00"
	if c.OrdinaryRules.AllowedHours[0] != "08:00-23:00" {
		t.Fatal("effective account mutates source configuration")
	}
}

func TestExampleAndLoad(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data, "yaml"); err == nil {
		t.Fatal("example model placeholder accepted")
	}
	data = []byte(strings.ReplaceAll(string(data), "REPLACE_WITH_SUPPORTED_MODEL", "supported"))
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.DailyRules) != 1 || c.DailyRules[0].ID != "morning" || c.Timezone != "Asia/Shanghai" {
		t.Fatal("example configuration contract changed")
	}
}
