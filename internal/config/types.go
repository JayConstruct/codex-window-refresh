// Package config loads and validates scheduler policy without host or network access.
package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"codex-window-refresh/internal/core"
)

type Duration time.Duration

var durationPattern = regexp.MustCompile(`^([0-9]+)([smhd])$`)

func (d Duration) Value() time.Duration { return time.Duration(d) }
func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	match := durationPattern.FindStringSubmatch(value)
	if match == nil {
		return fmt.Errorf("invalid duration %q: expected integer and s/m/h/d", value)
	}
	n, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return fmt.Errorf("duration overflow")
	}
	scale := map[string]uint64{"s": uint64(time.Second), "m": uint64(time.Minute), "h": uint64(time.Hour), "d": uint64(24 * time.Hour)}[match[2]]
	if n > uint64(1<<63-1)/scale {
		return fmt.Errorf("duration overflow")
	}
	*d = Duration(n * scale)
	return nil
}
func (d Duration) MarshalJSON() ([]byte, error) {
	if d < 0 || time.Duration(d)%time.Second != 0 {
		return nil, fmt.Errorf("duration is not a nonnegative whole second")
	}
	return json.Marshal(strconv.FormatInt(int64(d)/int64(time.Second), 10) + "s")
}

type DelayRange struct {
	Min Duration `json:"min"`
	Max Duration `json:"max"`
}
type ResetRule struct {
	Enabled     bool       `json:"enabled"`
	RandomDelay DelayRange `json:"random_delay"`
}
type WeeklyRule struct {
	Enabled             bool       `json:"enabled"`
	RandomDelay         DelayRange `json:"random_delay"`
	BypassActivityLimit bool       `json:"bypass_activity_limit"`
	BypassAllowedHours  bool       `json:"bypass_allowed_hours"`
}
type ResetRules struct {
	FiveHour ResetRule  `json:"five_hour"`
	Weekly   WeeklyRule `json:"weekly"`
}
type Activity struct {
	Lookback        Duration `json:"lookback"`
	AccountsPerUser float64  `json:"accounts_per_user"`
	MaxAccounts     int      `json:"max_accounts"`
	IdentitySource  string   `json:"identity_source"`
}
type OrdinaryRules struct {
	AllowedHours                     []string `json:"allowed_hours"`
	AllowOutsideHoursWhenUsersActive bool     `json:"allow_outside_hours_when_users_active"`
}
type DailyRule struct {
	ID                   string `json:"id"`
	Enabled              bool   `json:"enabled"`
	RandomTimeRange      string `json:"random_time_range"`
	MinimumReadyAccounts int    `json:"minimum_ready_accounts"`
	CatchUp              bool   `json:"catch_up"`
}

// UnmarshalJSON seeds defaults per new array element without coercing scalars.
func (r *DailyRule) UnmarshalJSON(data []byte) error {
	type plain DailyRule
	v := plain{Enabled: true, MinimumReadyAccounts: 1, CatchUp: true}
	if err := decodeStrict(data, &v); err != nil {
		return err
	}
	*r = DailyRule(v)
	return nil
}

type Execution struct {
	QuotaReadTimeout         Duration   `json:"quota_read_timeout"`
	RequestTimeout           Duration   `json:"request_timeout"`
	Model                    string     `json:"model"`
	Prompt                   string     `json:"prompt"`
	MaxConcurrency           int        `json:"max_concurrency"`
	MinDispatchSpacing       Duration   `json:"min_dispatch_spacing"`
	QuotaMaxAgeBeforeSend    Duration   `json:"quota_max_age_before_send"`
	VerifyDelays             []Duration `json:"verify_delays"`
	VerificationCapacityHold Duration   `json:"verification_capacity_hold"`
	CatchUp                  bool       `json:"catch_up"`
}
type AccountOverride struct {
	Paused                           *bool       `json:"paused,omitempty"`
	FiveHourEnabled                  *bool       `json:"five_hour_enabled,omitempty"`
	WeeklyEnabled                    *bool       `json:"weekly_enabled,omitempty"`
	FiveHourRandomDelay              *DelayRange `json:"five_hour_random_delay,omitempty"`
	WeeklyRandomDelay                *DelayRange `json:"weekly_random_delay,omitempty"`
	AllowedHours                     *[]string   `json:"allowed_hours,omitempty"`
	AllowOutsideHoursWhenUsersActive *bool       `json:"allow_outside_hours_when_users_active,omitempty"`
}
type Config struct {
	SchemaVersion    int                                `json:"schema_version"`
	Enabled          bool                               `json:"enabled"`
	Timezone         string                             `json:"timezone"`
	ResetRules       ResetRules                         `json:"reset_rules"`
	Activity         Activity                           `json:"activity"`
	OrdinaryRules    OrdinaryRules                      `json:"ordinary_rules"`
	DailyRules       []DailyRule                        `json:"daily_rules"`
	Execution        Execution                          `json:"execution"`
	AccountOverrides map[core.AccountID]AccountOverride `json:"account_overrides"`
}

func Defaults() Config {
	delay := DelayRange{Min: 0, Max: Duration(10 * time.Minute)}
	return Config{SchemaVersion: 1, Enabled: true, Timezone: "Asia/Shanghai",
		ResetRules:    ResetRules{FiveHour: ResetRule{Enabled: true, RandomDelay: delay}, Weekly: WeeklyRule{Enabled: true, RandomDelay: delay, BypassActivityLimit: true, BypassAllowedHours: true}},
		Activity:      Activity{Lookback: Duration(30 * time.Minute), AccountsPerUser: 1, MaxAccounts: 10, IdentitySource: "host_caller_scope"},
		OrdinaryRules: OrdinaryRules{AllowedHours: []string{"08:00-23:00"}, AllowOutsideHoursWhenUsersActive: true}, DailyRules: []DailyRule{},
		Execution: Execution{QuotaReadTimeout: Duration(15 * time.Second), RequestTimeout: Duration(time.Minute), Prompt: "Reply OK.", MaxConcurrency: 1, MinDispatchSpacing: Duration(5 * time.Second), QuotaMaxAgeBeforeSend: Duration(30 * time.Second), VerifyDelays: []Duration{Duration(5 * time.Second), Duration(30 * time.Second), Duration(2 * time.Minute)}, VerificationCapacityHold: Duration(15 * time.Minute), CatchUp: true}, AccountOverrides: map[core.AccountID]AccountOverride{}}
}

// EffectiveAccount keeps candidate eligibility separate from automatic event creation.
type EffectiveAccount struct {
	Paused                           bool
	OrdinaryParticipant              bool
	FiveHourEnabled                  bool
	WeeklyEnabled                    bool
	FiveHourRandomDelay              DelayRange
	WeeklyRandomDelay                DelayRange
	AllowedHours                     []string
	AllowOutsideHoursWhenUsersActive bool
}

func (c Config) Account(id core.AccountID) EffectiveAccount {
	o := c.AccountOverrides[id]
	result := EffectiveAccount{OrdinaryParticipant: true, WeeklyEnabled: c.ResetRules.Weekly.Enabled, FiveHourRandomDelay: c.ResetRules.FiveHour.RandomDelay, WeeklyRandomDelay: c.ResetRules.Weekly.RandomDelay, AllowedHours: append([]string{}, c.OrdinaryRules.AllowedHours...), AllowOutsideHoursWhenUsersActive: c.OrdinaryRules.AllowOutsideHoursWhenUsersActive}
	if o.Paused != nil {
		result.Paused = *o.Paused
	}
	if o.FiveHourEnabled != nil {
		result.OrdinaryParticipant = *o.FiveHourEnabled
	}
	result.FiveHourEnabled = c.ResetRules.FiveHour.Enabled && result.OrdinaryParticipant
	if o.WeeklyEnabled != nil {
		result.WeeklyEnabled = result.WeeklyEnabled && *o.WeeklyEnabled
	}
	if o.FiveHourRandomDelay != nil {
		result.FiveHourRandomDelay = *o.FiveHourRandomDelay
	}
	if o.WeeklyRandomDelay != nil {
		result.WeeklyRandomDelay = *o.WeeklyRandomDelay
	}
	if o.AllowedHours != nil {
		result.AllowedHours = append([]string{}, (*o.AllowedHours)...)
	}
	if o.AllowOutsideHoursWhenUsersActive != nil {
		result.AllowOutsideHoursWhenUsersActive = *o.AllowOutsideHoursWhenUsersActive
	}
	return result
}

type RuntimeConfig struct {
	Enabled                    bool
	Timezone                   string
	ActivityLookback           time.Duration
	AccountsPerUser            float64
	MaxAccounts                int
	IdentitySource             string
	Model, Prompt              string
	MaxConcurrency             int
	MinDispatchSpacing         time.Duration
	EvidenceMaxAge             time.Duration
	ForceObserveBeforeDispatch bool
	VerifyDelays               []time.Duration
	VerificationCapacityHold   time.Duration
}

func (c Config) Runtime() RuntimeConfig {
	age := c.Execution.QuotaMaxAgeBeforeSend.Value()
	force := age == 0
	if force {
		age = 30 * time.Second
	}
	delays := make([]time.Duration, len(c.Execution.VerifyDelays))
	for i, v := range c.Execution.VerifyDelays {
		delays[i] = v.Value()
	}
	return RuntimeConfig{Enabled: c.Enabled, Timezone: c.Timezone, ActivityLookback: c.Activity.Lookback.Value(), AccountsPerUser: c.Activity.AccountsPerUser, MaxAccounts: c.Activity.MaxAccounts, IdentitySource: c.Activity.IdentitySource, Model: c.Execution.Model, Prompt: c.Execution.Prompt, MaxConcurrency: c.Execution.MaxConcurrency, MinDispatchSpacing: c.Execution.MinDispatchSpacing.Value(), EvidenceMaxAge: age, ForceObserveBeforeDispatch: force, VerifyDelays: delays, VerificationCapacityHold: c.Execution.VerificationCapacityHold.Value()}
}

// Location is only usable after Validate; invalid configurations return nil.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil
	}
	return loc
}
