package cpa

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"codex-window-refresh/internal/core"
)

var ErrQuotaMalformed = errors.New("quota_response_unrecognized")

type quotaWindow struct {
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	WindowMinutes      *int64   `json:"window_minutes"`
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            *int64   `json:"reset_at"`
	ResetAfter         *int64   `json:"reset_after_seconds"`
	ResetAfterLegacy   *int64   `json:"reset_after"`
}
type rateLimit struct {
	Allowed        *bool        `json:"allowed"`
	LimitReached   *bool        `json:"limit_reached"`
	Primary        *quotaWindow `json:"primary_window"`
	Secondary      *quotaWindow `json:"secondary_window"`
	PrimaryEvent   *quotaWindow `json:"primary"`
	SecondaryEvent *quotaWindow `json:"secondary"`
}

func ParseQuota(raw []byte, read core.QuotaRead, at time.Time, class core.ObservationClass, baseline core.AccountState) (core.QuotaObservation, error) {
	var data struct {
		RateLimit      *rateLimit `json:"rate_limit"`
		RateLimits     *rateLimit `json:"rate_limits"`
		RateLimitCamel *rateLimit `json:"rateLimit"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return core.QuotaObservation{}, ErrQuotaMalformed
	}
	rate := data.RateLimit
	for _, alias := range []*rateLimit{data.RateLimits, data.RateLimitCamel} {
		if rate != nil && alias != nil && !reflect.DeepEqual(rate, alias) {
			return core.QuotaObservation{}, ErrQuotaMalformed
		}
		if rate == nil {
			rate = alias
		}
	}
	if rate == nil {
		return core.QuotaObservation{}, ErrQuotaMalformed
	}
	observation := core.QuotaObservation{AccountID: read.AccountID, Generation: read.Generation, ReadStartedSeq: read.ReadStartedSeq, ReceivedAt: at, Source: "cpa_wham_usage", Class: class}
	if class == core.ObservationExecution {
		observation.Source = "cpa_execution_quota"
	}
	if rate.Allowed != nil {
		p := core.PermissionBlocked
		if *rate.Allowed && (rate.LimitReached == nil || !*rate.LimitReached) {
			p = core.PermissionAllowed
		}
		observation.Permission = &p
	} else if rate.LimitReached != nil && *rate.LimitReached {
		p := core.PermissionBlocked
		observation.Permission = &p
	}
	windows := []*quotaWindow{rate.Primary, rate.Secondary, rate.PrimaryEvent, rate.SecondaryEvent}
	for _, w := range windows {
		if w == nil {
			continue
		}
		kind, e, err := classifyWindow(*w, read, at, class, baseline)
		if err != nil {
			return core.QuotaObservation{}, err
		}
		if kind == core.FiveHour {
			if observation.FiveHour != nil {
				return core.QuotaObservation{}, ErrQuotaMalformed
			}
			observation.FiveHour = e
		}
		if kind == core.Weekly {
			if observation.Weekly != nil {
				return core.QuotaObservation{}, ErrQuotaMalformed
			}
			observation.Weekly = e
		}
	}
	if observation.Permission == nil && observation.FiveHour == nil && observation.Weekly == nil {
		return core.QuotaObservation{}, ErrQuotaMalformed
	}
	return observation, nil
}
func classifyWindow(w quotaWindow, read core.QuotaRead, at time.Time, class core.ObservationClass, baseline core.AccountState) (core.WindowKind, *core.WindowEvidence, error) {
	seconds := int64(0)
	if w.LimitWindowSeconds != nil {
		seconds = *w.LimitWindowSeconds
		if w.WindowMinutes != nil && (seconds%60 != 0 || *w.WindowMinutes != seconds/60) {
			return "", nil, ErrQuotaMalformed
		}
	} else if w.WindowMinutes != nil && *w.WindowMinutes > 0 && *w.WindowMinutes <= 10080 {
		seconds = *w.WindowMinutes * 60
	}
	kind := core.WindowKind("")
	switch seconds {
	case 18000:
		kind = core.FiveHour
	case 604800:
		kind = core.Weekly
	default:
		return "", nil, nil
	}
	if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
		return "", nil, ErrQuotaMalformed
	}
	// Both representations must respect this particular window. The tolerance
	// covers integer rounding and response sampling; it does not extend expiry.
	const resetTolerance = time.Minute
	after := w.ResetAfter
	if after != nil && (*after < 0 || *after > seconds+int64(resetTolerance/time.Second)) {
		return "", nil, ErrQuotaMalformed
	}
	var reset time.Time
	if w.ResetAt != nil {
		if *w.ResetAt <= 0 {
			return "", nil, ErrQuotaMalformed
		}
		reset = time.Unix(*w.ResetAt, 0).UTC()
		if reset.Year() < 1970 || reset.Year() > 9999 || reset.After(at.Add(time.Duration(seconds)*time.Second+resetTolerance)) {
			return "", nil, ErrQuotaMalformed
		}
		if after != nil {
			relativeReset := at.Add(time.Duration(*after) * time.Second)
			// A positive countdown contradicts an expired absolute boundary even
			// inside the sampling tolerance: accepting expiry would permit a send.
			if *after > 0 && !reset.After(at) {
				return "", nil, ErrQuotaMalformed
			}
			// An exhausted countdown may be clamped to zero long after expiry.
			if (*after > 0 || reset.After(at)) && (reset.Sub(relativeReset) > resetTolerance || relativeReset.Sub(reset) > resetTolerance) {
				return "", nil, ErrQuotaMalformed
			}
		}
	} else {
		if after == nil {
			return "", nil, ErrQuotaMalformed
		}
		reset = at.Add(time.Duration(*after) * time.Second)
	}
	state := core.WindowUnknown
	if !reset.After(at) {
		state = core.WindowExpired
	} else if *w.UsedPercent > 0 {
		state = core.WindowActive
	} else if class == core.ObservationQuery && baseline.ID == read.AccountID && baseline.Generation == read.Generation {
		prior, stable := baseline.FiveHour, baseline.FiveHourBaseline
		if kind == core.Weekly {
			prior, stable = baseline.Weekly, baseline.WeeklyBaseline
		}
		// This preserves an already confirmed active cycle; it never declares a new
		// zero-used future window active merely because an old cycle has expired.
		if prior.State == core.WindowActive && prior.ResetAt.Equal(reset) && stable.StableResetAt.Equal(reset) && at.Before(reset) {
			state = core.WindowActive
		}
	}
	// Query zero-usage future boundaries may be unactivated moving placeholders.
	// Even an expired baseline cannot alone prove that the current value is inactive.
	// After a completed plugin generation, a future boundary remains subject to
	// normal evidence verification; we do not treat successful HTTP as activation.
	b := baseline.FiveHourBaseline
	if kind == core.Weekly {
		b = baseline.WeeklyBaseline
	}
	cycle := ""
	if state != core.WindowUnknown {
		cycle = string(kind) + ":" + strconv.FormatInt(reset.Unix(), 10)
		if b.StableResetAt.Equal(reset) && b.CycleID != "" {
			cycle = b.CycleID
		}
	} else {
		cycle = b.CycleID
	}
	evidence := &core.WindowEvidence{Kind: kind, State: state, CycleID: cycle, ResetAt: reset, Remaining: &core.QuotaAmount{Value: 100 - *w.UsedPercent, Unit: "percent"}, ConfirmedAt: at, Source: "cpa_wham_usage", ReadStartedSeq: read.ReadStartedSeq}
	if class == core.ObservationExecution {
		evidence.Source = "cpa_execution_quota"
	}
	return kind, evidence, nil
}
func quotaHeaders(headers http.Header) []byte {
	rate := rateLimit{}
	if v := headers.Get("X-Codex-Allowed"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			rate.Allowed = &b
		}
	}
	if v := headers.Get("X-Codex-Limit-Reached"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			rate.LimitReached = &b
		}
	}
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		used, err := strconv.ParseFloat(headers.Get(prefix+"Used-Percent"), 64)
		if err != nil {
			continue
		}
		minutes, err := strconv.ParseInt(headers.Get(prefix+"Window-Minutes"), 10, 64)
		if err != nil {
			continue
		}
		w := &quotaWindow{UsedPercent: &used, WindowMinutes: &minutes}
		if value, err := strconv.ParseInt(headers.Get(prefix+"Reset-At"), 10, 64); err == nil {
			w.ResetAt = &value
		}
		if value, err := strconv.ParseInt(headers.Get(prefix+"Reset-After-Seconds"), 10, 64); err == nil {
			w.ResetAfter = &value
		}
		if name == "Primary" {
			rate.Primary = w
		} else {
			rate.Secondary = w
		}
	}
	raw, _ := json.Marshal(struct {
		RateLimit rateLimit `json:"rate_limit"`
	}{rate})
	return raw
}

// Websocket rate-limit frames use snake_case or camelCase in official CPA's
// quota normalizer. Keep those aliases at the adapter boundary only.
func (w *quotaWindow) UnmarshalJSON(raw []byte) error {
	type plain quotaWindow
	var value struct {
		plain
		LimitWindowSecondsCamel *int64   `json:"limitWindowSeconds"`
		WindowMinutesCamel      *int64   `json:"windowMinutes"`
		UsedPercentCamel        *float64 `json:"usedPercent"`
		ResetAtCamel            *int64   `json:"resetAt"`
		ResetAfterCamel         *int64   `json:"resetAfterSeconds"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*w = quotaWindow(value.plain)
	var err error
	if w.LimitWindowSeconds, err = quotaAlias(w.LimitWindowSeconds, value.LimitWindowSecondsCamel); err != nil {
		return err
	}
	if w.WindowMinutes, err = quotaAlias(w.WindowMinutes, value.WindowMinutesCamel); err != nil {
		return err
	}
	if w.UsedPercent, err = quotaAlias(w.UsedPercent, value.UsedPercentCamel); err != nil {
		return err
	}
	if w.ResetAt, err = quotaAlias(w.ResetAt, value.ResetAtCamel); err != nil {
		return err
	}
	if w.ResetAfter, err = quotaAlias(w.ResetAfter, value.ResetAfterCamel); err != nil {
		return err
	}
	if w.ResetAfter, err = quotaAlias(w.ResetAfter, w.ResetAfterLegacy); err != nil {
		return err
	}
	w.ResetAfterLegacy = nil
	return nil
}
func (r *rateLimit) UnmarshalJSON(raw []byte) error {
	type plain rateLimit
	var value struct {
		plain
		LimitReachedCamel *bool `json:"limitReached"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = rateLimit(value.plain)
	var err error
	if r.LimitReached, err = quotaAlias(r.LimitReached, value.LimitReachedCamel); err != nil {
		return err
	}
	for _, pair := range []struct {
		primary **quotaWindow
		alias   **quotaWindow
	}{{&r.Primary, &r.PrimaryEvent}, {&r.Secondary, &r.SecondaryEvent}} {
		if *pair.primary != nil && *pair.alias != nil && !reflect.DeepEqual(*pair.primary, *pair.alias) {
			return ErrQuotaMalformed
		}
		if *pair.primary == nil {
			*pair.primary = *pair.alias
		}
		*pair.alias = nil
	}
	return nil
}

func quotaAlias[T comparable](primary, alias *T) (*T, error) {
	if primary != nil && alias != nil && *primary != *alias {
		return nil, ErrQuotaMalformed
	}
	if primary != nil {
		return primary, nil
	}
	return alias, nil
}
