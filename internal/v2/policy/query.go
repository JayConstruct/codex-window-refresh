package policy

import (
	"errors"
	"strings"
	"time"
)

type Delay struct {
	Min int `json:"min_seconds"`
	Max int `json:"max_seconds"`
}

type QueryConfig struct {
	Timezone      string   `json:"timezone"`
	Automatic     bool     `json:"automatic"`
	Interval      int      `json:"interval_seconds"`
	FiveHour      bool     `json:"on_five_hour_expiry"`
	Weekly        bool     `json:"on_weekly_expiry"`
	Delay         Delay    `json:"random_delay"`
	ExcludedHours []string `json:"excluded_hours"`
}

var ErrInvalid = errors.New("invalid_query_settings")

func DefaultQuery() QueryConfig {
	return QueryConfig{Timezone: "Asia/Shanghai", Interval: 300, Delay: Delay{Min: 5, Max: 60}, ExcludedHours: []string{}}
}

func spanMinutes(value string) (int, int, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 || len(parts[0]) != 5 || len(parts[1]) != 5 || parts[0][2] != ':' || parts[1][2] != ':' {
		return 0, 0, ErrInvalid
	}
	start, err := time.Parse("15:04", parts[0])
	if err != nil {
		return 0, 0, ErrInvalid
	}
	first := start.Hour()*60 + start.Minute()
	if parts[1] == "24:00" {
		return first, 1440, nil
	}
	end, err := time.Parse("15:04", parts[1])
	if err != nil || first == end.Hour()*60+end.Minute() {
		return 0, 0, ErrInvalid
	}
	return first, end.Hour()*60 + end.Minute(), nil
}

func (config QueryConfig) Validate() error {
	if config.Timezone == "" || config.Timezone == "Local" || len(config.Timezone) > 128 {
		return ErrInvalid
	}
	if _, err := time.LoadLocation(config.Timezone); err != nil {
		return ErrInvalid
	}
	if config.Interval < 30 || config.Interval > 3600 || config.Delay.Min < 0 || config.Delay.Max < config.Delay.Min || config.Delay.Max > 86400 || len(config.ExcludedHours) > 24 {
		return ErrInvalid
	}
	covered := [1440]bool{}
	for _, span := range config.ExcludedHours {
		first, last, err := spanMinutes(span)
		if err != nil {
			return err
		}
		for minute := 0; minute < 1440; minute++ {
			inside := first < last && minute >= first && minute < last || first > last && (minute >= first || minute < last)
			if inside && covered[minute] {
				return ErrInvalid
			}
			if inside {
				covered[minute] = true
			}
		}
	}
	return nil
}

func (config QueryConfig) Enabled(trigger string) bool {
	switch trigger {
	case "periodic":
		return config.Automatic
	case "five_hour_expiry":
		return config.FiveHour
	case "weekly_expiry":
		return config.Weekly
	case "initial_sync":
		return config.Automatic || config.FiveHour || config.Weekly
	}
	return false
}

func (config QueryConfig) Allowed(at time.Time) bool {
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return false
	}
	local := at.In(location)
	minute := local.Hour()*60 + local.Minute()
	for _, span := range config.ExcludedHours {
		first, last, err := spanMinutes(span)
		if err != nil {
			return false
		}
		if first < last && minute >= first && minute < last || first > last && (minute >= first || minute < last) {
			return false
		}
	}
	return true
}
