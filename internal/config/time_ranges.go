package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-window-refresh/internal/core"
)

// MinuteRange contains minute offsets from local midnight; End may equal 1440.
// Daily equal endpoints represent an exact time. Allowed ranges are half-open.
type MinuteRange struct{ Start, End int }

func parseMinute(value string, allow24 bool) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, fmt.Errorf("expected HH:mm")
	}
	for _, i := range []int{0, 1, 3, 4} {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("expected HH:mm")
		}
	}
	hour, _ := strconv.Atoi(value[:2])
	minute, _ := strconv.Atoi(value[3:])
	if minute > 59 || hour > 23 {
		if allow24 && hour == 24 && minute == 0 {
			return 1440, nil
		}
		return 0, fmt.Errorf("time outside permitted range")
	}
	return hour*60 + minute, nil
}
func parseRange(value string) (MinuteRange, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return MinuteRange{}, fmt.Errorf("expected HH:mm-HH:mm")
	}
	start, err := parseMinute(parts[0], false)
	if err != nil {
		return MinuteRange{}, err
	}
	end, err := parseMinute(parts[1], true)
	if err != nil {
		return MinuteRange{}, err
	}
	return MinuteRange{Start: start, End: end}, nil
}
func (r DailyRule) MinuteRange() (MinuteRange, error) {
	result, err := parseRange(r.RandomTimeRange)
	if err != nil {
		return MinuteRange{}, err
	}
	if result.End < result.Start {
		return MinuteRange{}, fmt.Errorf("daily range cannot cross midnight")
	}
	return result, nil
}

// AllowedRanges normalizes crossing-midnight segments and merges adjacency.
// It rejects overlaps rather than silently changing the configured policy.
func AllowedRanges(values []string) ([]MinuteRange, error) {
	result := []MinuteRange{}
	for _, value := range values {
		r, err := parseRange(value)
		if err != nil {
			return nil, err
		}
		if r.Start == r.End {
			return nil, fmt.Errorf("empty allowed interval %q", value)
		}
		if r.End < r.Start {
			result = append(result, MinuteRange{Start: r.Start, End: 1440})
			if r.End > 0 {
				result = append(result, MinuteRange{Start: 0, End: r.End})
			}
		} else {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Start < result[j].Start })
	merged := []MinuteRange{}
	for _, r := range result {
		if len(merged) == 0 {
			merged = append(merged, r)
			continue
		}
		prev := &merged[len(merged)-1]
		if r.Start < prev.End {
			return nil, fmt.Errorf("allowed interval overlap")
		}
		if r.Start == prev.End {
			prev.End = r.End
		} else {
			merged = append(merged, r)
		}
	}
	return merged, nil
}
func (c Config) WithinAllowedHours(at time.Time, id core.AccountID) bool {
	loc := c.Location()
	if loc == nil {
		return false
	}
	at = at.In(loc)
	ranges, err := AllowedRanges(c.Account(id).AllowedHours)
	if err != nil {
		return false
	}
	minute := at.Hour()*60 + at.Minute()
	for _, r := range ranges {
		if minute >= r.Start && minute < r.End {
			return true
		}
	}
	return false
}
