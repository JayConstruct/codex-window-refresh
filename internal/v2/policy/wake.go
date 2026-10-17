package policy

import (
	"errors"
	"math"
	"strings"
	"time"
)

var ErrInvalidWake = errors.New("invalid_wake_settings")

type WakeConfig struct {
	Enabled         bool     `json:"enabled"`
	FiveHour        bool     `json:"on_five_hour_reset"`
	Weekly          bool     `json:"on_weekly_reset"`
	Timezone        string   `json:"timezone"`
	Delay           Delay    `json:"random_delay"`
	AllowedHours    []string `json:"allowed_hours"`
	AccountsPerUser float64  `json:"accounts_per_user"`
	MaxAccounts     int      `json:"max_accounts"`
	DailyEnabled    bool     `json:"daily_enabled"`
	DailyWindow     string   `json:"daily_window"`
	DailyTarget     int      `json:"daily_target"`
}

func DefaultWake() WakeConfig {
	return WakeConfig{Timezone: "Asia/Shanghai", Delay: Delay{Min: 5, Max: 60}, AllowedHours: []string{"08:00-23:00"}, AccountsPerUser: 1, MaxAccounts: 10000, DailyWindow: "09:00-09:10", DailyTarget: 1}
}

func (config WakeConfig) Normalize() WakeConfig {
	config.AllowedHours = append([]string{}, config.AllowedHours...)
	return config
}

func (config WakeConfig) Validate() error {
	query := DefaultQuery()
	query.Timezone, query.Delay, query.ExcludedHours = config.Timezone, config.Delay, config.AllowedHours
	first, last, err := spanMinutes(config.DailyWindow)
	if query.Validate() != nil || err != nil || first >= last || config.MaxAccounts < 1 || config.MaxAccounts > 10000 || math.IsNaN(config.AccountsPerUser) || math.IsInf(config.AccountsPerUser, 0) || config.AccountsPerUser < 0 || config.AccountsPerUser > 100 || config.DailyTarget < 0 || config.DailyTarget > config.MaxAccounts || config.Enabled && !config.FiveHour && !config.Weekly && !config.DailyEnabled {
		return ErrInvalidWake
	}
	return nil
}

func (config WakeConfig) RuleEnabled(reason string) bool {
	if reason == "manual" {
		return true
	}
	if !config.Enabled {
		return false
	}
	switch reason {
	case "five_hour_reset":
		return config.FiveHour
	case "weekly_reset":
		return config.Weekly
	case "daily":
		return config.DailyEnabled && config.DailyTarget > 0
	}
	return false
}

func (config WakeConfig) InAllowedHours(at time.Time) bool {
	if len(config.AllowedHours) == 0 {
		return true
	}
	query := DefaultQuery()
	query.Timezone, query.ExcludedHours = config.Timezone, config.AllowedHours
	return !query.Allowed(at)
}

func (config WakeConfig) DailyBounds(at time.Time) (string, time.Time, time.Time, bool) {
	location, err := time.LoadLocation(config.Timezone)
	first, last, spanErr := spanMinutes(config.DailyWindow)
	if err != nil || spanErr != nil || first >= last {
		return "", time.Time{}, time.Time{}, false
	}
	local := at.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), first/60, first%60, 0, 0, location)
	end := time.Date(local.Year(), local.Month(), local.Day(), last/60, last%60, 0, 0, location)
	return local.Format("2006-01-02"), start, end, !at.Before(start)
}

func WakeModel(config ModelConfig, reason string) (string, string) {
	model, prompt := config.DefaultModel, config.DefaultPrompt
	if reason == "weekly_reset" {
		if config.WeeklyModel != "" {
			model = config.WeeklyModel
		}
		if strings.TrimSpace(config.WeeklyPrompt) != "" {
			prompt = config.WeeklyPrompt
		}
	} else if reason == "five_hour_reset" {
		if config.FiveHourModel != "" {
			model = config.FiveHourModel
		}
		if strings.TrimSpace(config.FiveHourPrompt) != "" {
			prompt = config.FiveHourPrompt
		}
	}
	return model, prompt
}
