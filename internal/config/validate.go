package config

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ValidationError struct {
	Fields []FieldError `json:"fields"`
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "invalid configuration"
	}
	f := e.Fields[0]
	return fmt.Sprintf("%s: %s (%s)", f.Path, f.Message, f.Code)
}

var dailyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (c Config) Validate() error {
	errors := &ValidationError{}
	add := func(path, code, message string) {
		errors.Fields = append(errors.Fields, FieldError{Path: path, Code: code, Message: message})
	}
	bound := func(path string, v, min, max int) {
		if v < min || v > max {
			add(path, "OUT_OF_RANGE", fmt.Sprintf("must be between %d and %d", min, max))
		}
	}
	duration := func(path string, v Duration, min, max time.Duration) {
		if v.Value() < min || v.Value() > max || v.Value()%time.Second != 0 {
			add(path, "INVALID_DURATION", fmt.Sprintf("must be a whole-second duration between %s and %s", min, max))
		}
	}
	delay := func(path string, v DelayRange) {
		duration(path+".min", v.Min, 0, 24*time.Hour)
		duration(path+".max", v.Max, 0, 24*time.Hour)
		if v.Min > v.Max {
			add(path, "DELAY_RANGE_INVERTED", "min cannot exceed max")
		}
	}
	allowed := func(path string, values []string) {
		if _, err := AllowedRanges(values); err != nil {
			code := "INVALID_TIME_RANGE"
			if strings.Contains(err.Error(), "overlap") {
				code = "ALLOWED_INTERVAL_OVERLAP"
			}
			if strings.Contains(err.Error(), "empty") {
				code = "EMPTY_ALLOWED_INTERVAL"
			}
			add(path, code, err.Error())
		}
	}
	if c.SchemaVersion != 1 {
		add("schema_version", "UNSUPPORTED_VERSION", "only schema version 1 is supported")
	}
	if c.Timezone == "" || c.Timezone == "Local" || c.Location() == nil {
		add("timezone", "INVALID_TIMEZONE", "must name a loadable IANA time zone")
	}
	delay("reset_rules.five_hour.random_delay", c.ResetRules.FiveHour.RandomDelay)
	delay("reset_rules.weekly.random_delay", c.ResetRules.Weekly.RandomDelay)
	if !c.ResetRules.Weekly.BypassActivityLimit {
		add("reset_rules.weekly.bypass_activity_limit", "WEEKLY_EXEMPTION_REQUIRED", "must be true")
	}
	if !c.ResetRules.Weekly.BypassAllowedHours {
		add("reset_rules.weekly.bypass_allowed_hours", "WEEKLY_EXEMPTION_REQUIRED", "must be true")
	}
	duration("activity.lookback", c.Activity.Lookback, time.Minute, 24*time.Hour)
	duration("quota_query.interval", c.QuotaQuery.Interval, 30*time.Second, time.Hour)
	duration("quota_query.five_hour_recent_usage.lookback", c.QuotaQuery.FiveHourRecentUsage.Lookback, time.Minute, 7*24*time.Hour)
	if c.Execution.PromptMode != "fixed" && c.Execution.PromptMode != "random" {
		add("execution.prompt_mode", "INVALID_PROMPT_MODE", "must be fixed or random")
	}
	for _, pool := range []struct {
		path     string
		values   []string
		required bool
	}{
		{"execution.prompt_pool", c.Execution.PromptPool, c.Execution.PromptMode == "random"},
		{"reset_rules.five_hour.prompt_pool", c.ResetRules.FiveHour.PromptPool, false},
		{"reset_rules.weekly.prompt_pool", c.ResetRules.Weekly.PromptPool, false},
	} {
		if err := ValidatePromptPool(pool.values, pool.required); err != nil {
			add(pool.path, "INVALID_PROMPT_POOL", err.Error())
		}
	}
	for _, window := range []struct {
		path  string
		query WindowQuery
	}{{"quota_query.five_hour", c.QuotaQuery.FiveHour}, {"quota_query.weekly", c.QuotaQuery.Weekly}} {
		if window.query.Interval != 0 {
			duration(window.path+".interval", window.query.Interval, 30*time.Second, time.Hour)
			if window.query.Interval < c.QuotaQuery.Interval {
				add(window.path+".interval", "QUERY_INTERVAL_BELOW_SHARED", "cannot be shorter than quota_query.interval; use 0s to inherit")
			}
		}
		for _, timing := range []struct {
			path  string
			value QueryTiming
		}{{"expiry", window.query.Expiry}, {"periodic", window.query.Periodic}} {
			allowed(window.path+"."+timing.path+".excluded_hours", timing.value.ExcludedHours)
			delay(window.path+"."+timing.path+".random_delay", timing.value.RandomDelay)
		}
	}
	bound("planning.horizon_days", c.Planning.HorizonDays, 1, 30)
	ratio := c.Activity.AccountsPerUser
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		add("activity.accounts_per_user", "NON_FINITE_NUMBER", "must be a finite number")
	} else if ratio < 0 || ratio > 100 {
		add("activity.accounts_per_user", "OUT_OF_RANGE", "must be between 0 and 100")
	}
	bound("activity.max_accounts", c.Activity.MaxAccounts, 0, 100000)
	switch c.Activity.IdentitySource {
	case "host_caller_scope", "host_user", "mapped_user", "api_key_hash":
	default:
		add("activity.identity_source", "INVALID_IDENTITY_SOURCE", "unsupported identity source")
	}
	allowed("ordinary_rules.allowed_hours", c.OrdinaryRules.AllowedHours)
	bound("daily_rules", len(c.DailyRules), 0, 100)
	seen := map[string]bool{}
	idPattern := dailyIDPattern
	for i, r := range c.DailyRules {
		path := fmt.Sprintf("daily_rules[%d]", i)
		if !idPattern.MatchString(r.ID) {
			add(path+".id", "INVALID_RULE_ID", "must contain 1 to 64 letters, numbers, underscores or hyphens")
		}
		if seen[r.ID] {
			add(path+".id", "DUPLICATE_RULE_ID", "rule id must be unique")
		}
		seen[r.ID] = true
		if _, err := r.MinuteRange(); err != nil {
			code := "INVALID_TIME_RANGE"
			if strings.Contains(err.Error(), "midnight") {
				code = "DAILY_RANGE_CROSSES_MIDNIGHT"
			}
			add(path+".random_time_range", code, err.Error())
		}
		if r.MinimumReadyAccounts < 0 {
			add(path+".minimum_ready_accounts", "OUT_OF_RANGE", "must be nonnegative")
		} else if r.MinimumReadyAccounts > c.Activity.MaxAccounts {
			add(path+".minimum_ready_accounts", "MINIMUM_EXCEEDS_MAX_ACCOUNTS", "must not exceed activity.max_accounts")
		}
		if !r.CatchUp {
			add(path+".catch_up", "CATCH_UP_REQUIRED", "must be true")
		}
	}
	model := c.Execution.Model
	needsFiveHour := c.ResetRules.FiveHour.Enabled
	if c.OrdinaryRules.DailyEnabled {
		for _, rule := range c.DailyRules {
			needsFiveHour = needsFiveHour || rule.Enabled
		}
	}
	fiveHourModel, _ := c.WakeContent("five_hour_reset")
	weeklyModel, _ := c.WakeContent("weekly_reset")
	if c.Enabled && ((needsFiveHour && strings.TrimSpace(fiveHourModel) == "") || (c.ResetRules.Weekly.Enabled && strings.TrimSpace(weeklyModel) == "")) {
		add("execution.model", "MODEL_REQUIRED", "provide a default model or a model for every enabled wake rule")
	}
	if len(model) > 256 || !utf8.ValidString(model) {
		add("execution.model", "OUT_OF_RANGE", "must be valid UTF-8 and at most 256 bytes")
	}
	if strings.Contains(strings.ToUpper(model), "REPLACE_WITH") {
		add("execution.model", "MODEL_PLACEHOLDER", "replace the example model placeholder")
	}
	if strings.TrimSpace(c.Execution.Prompt) == "" || len(c.Execution.Prompt) > 4096 || !utf8.ValidString(c.Execution.Prompt) {
		add("execution.prompt", "OUT_OF_RANGE", "must be nonempty valid UTF-8 and at most 4096 bytes")
	}
	for _, content := range []struct{ path, model, prompt string }{
		{"reset_rules.five_hour", c.ResetRules.FiveHour.Model, c.ResetRules.FiveHour.Prompt},
		{"reset_rules.weekly", c.ResetRules.Weekly.Model, c.ResetRules.Weekly.Prompt},
	} {
		if content.model != "" && (strings.TrimSpace(content.model) == "" || len(content.model) > 256 || !utf8.ValidString(content.model)) {
			add(content.path+".model", "OUT_OF_RANGE", "must be empty or a valid model name of at most 256 bytes")
		}
		if strings.Contains(strings.ToUpper(content.model), "REPLACE_WITH") {
			add(content.path+".model", "MODEL_PLACEHOLDER", "replace the example model placeholder")
		}
		if content.prompt != "" && (strings.TrimSpace(content.prompt) == "" || len(content.prompt) > 4096 || !utf8.ValidString(content.prompt)) {
			add(content.path+".prompt", "OUT_OF_RANGE", "must be empty or valid content of at most 4096 bytes")
		}
	}
	bound("execution.max_concurrency", c.Execution.MaxConcurrency, 1, 32)
	duration("execution.quota_read_timeout", c.Execution.QuotaReadTimeout, time.Second, 5*time.Minute)
	duration("execution.request_timeout", c.Execution.RequestTimeout, time.Second, 30*time.Minute)
	duration("execution.min_dispatch_spacing", c.Execution.MinDispatchSpacing, 0, time.Hour)
	duration("execution.quota_max_age_before_send", c.Execution.QuotaMaxAgeBeforeSend, 0, 5*time.Minute)
	bound("execution.verify_delays", len(c.Execution.VerifyDelays), 1, 10)
	maximum := Duration(0)
	for i, v := range c.Execution.VerifyDelays {
		path := fmt.Sprintf("execution.verify_delays[%d]", i)
		duration(path, v, time.Second, time.Hour)
		if i > 0 && v <= c.Execution.VerifyDelays[i-1] {
			add(path, "VERIFY_DELAYS_NOT_INCREASING", "delays must strictly increase")
		}
		if v > maximum {
			maximum = v
		}
	}
	duration("execution.verification_capacity_hold", c.Execution.VerificationCapacityHold, time.Minute, 24*time.Hour)
	if c.Execution.VerificationCapacityHold < maximum {
		add("execution.verification_capacity_hold", "OUT_OF_RANGE", "cannot be shorter than maximum verification delay")
	}
	if !c.Execution.CatchUp {
		add("execution.catch_up", "CATCH_UP_REQUIRED", "must be true")
	}
	bound("account_overrides", len(c.AccountOverrides), 0, 100000)
	for id, o := range c.AccountOverrides {
		path := fmt.Sprintf("account_overrides[%q]", id)
		if strings.TrimSpace(string(id)) == "" || !utf8.ValidString(string(id)) {
			add(path, "INVALID_ACCOUNT_ID", "stable account id must be nonempty valid UTF-8")
		}
		if o.FiveHourRandomDelay != nil {
			delay(path+".five_hour_random_delay", *o.FiveHourRandomDelay)
		}
		if o.WeeklyRandomDelay != nil {
			delay(path+".weekly_random_delay", *o.WeeklyRandomDelay)
		}
		if o.AllowedHours != nil {
			allowed(path+".allowed_hours", *o.AllowedHours)
		}
	}
	if len(errors.Fields) > 0 {
		return errors
	}
	return nil
}
