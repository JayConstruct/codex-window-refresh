// Package quota normalizes evidence without importing the old runtime or state.
package quota

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	"codex-window-refresh/internal/v2/policy"
)

type Stamp struct {
	Source        string    `json:"source"`
	ObservedAt    time.Time `json:"observed_at"`
	ReadStartedAt time.Time `json:"read_started_at"`
}
type Snapshot struct {
	Source        string               `json:"source"`
	Binding       string               `json:"binding"`
	ObservedAt    time.Time            `json:"observed_at"`
	ReadStartedAt time.Time            `json:"read_started_at"`
	Permission    *core.Permission     `json:"permission,omitempty"`
	FiveHour      *core.WindowEvidence `json:"five_hour,omitempty"`
	Weekly        *core.WindowEvidence `json:"weekly,omitempty"`
	ResetCredits  *policy.ResetCredits `json:"reset_credits,omitempty"`
	Fields        map[string]Stamp     `json:"fields,omitempty"`
}

func Normalize(raw []byte, headers http.Header, binding, source string, at, started time.Time) (Snapshot, error) {
	if headers != nil {
		raw = cpa.QuotaHeaders(headers)
	}
	class := core.ObservationExecution
	if source == "cpa_wham_usage" {
		class = core.ObservationQuery
	}
	o, err := cpa.ParseQuota(raw, core.QuotaRead{}, at, class, core.AccountState{})
	credits := resetCount(raw)
	if err != nil && credits == nil {
		return Snapshot{}, err
	}
	s := Snapshot{Source: source, Binding: binding, ObservedAt: at, ReadStartedAt: started, Permission: o.Permission, FiveHour: o.FiveHour, Weekly: o.Weekly, ResetCredits: credits}
	for _, w := range []*core.WindowEvidence{s.FiveHour, s.Weekly} {
		if w != nil {
			w.Source = source
		}
	}
	return s, nil
}

// Counts reported by usage reads or bound native quota events share the same
// field ordering. Missing counts never erase earlier observations or imply zero.
func resetCount(raw []byte) *policy.ResetCredits {
	type count struct {
		Snake *int `json:"available_count"`
		Camel *int `json:"availableCount"`
	}
	var payload struct {
		Snake *count `json:"rate_limit_reset_credits"`
		Camel *count `json:"rateLimitResetCredits"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	var value *int
	for _, item := range []*count{payload.Snake, payload.Camel} {
		if item == nil {
			continue
		}
		for _, candidate := range []*int{item.Snake, item.Camel} {
			if candidate == nil {
				continue
			}
			if *candidate < 0 || *candidate > 100000 || value != nil && *value != *candidate {
				return nil
			}
			value = candidate
		}
	}
	if value == nil {
		return nil
	}
	return &policy.ResetCredits{AvailableCount: *value}
}

// Merge compares each field's read boundary independently. A query begun before
// an execution observation cannot overwrite it merely by finishing later.
func Merge(oldRaw, newRaw []byte) ([]byte, bool, error) {
	var incoming, current Snapshot
	if json.Unmarshal(newRaw, &incoming) != nil || incoming.Binding == "" || incoming.ObservedAt.IsZero() || incoming.ReadStartedAt.IsZero() || incoming.ReadStartedAt.After(incoming.ObservedAt) {
		return nil, false, errors.New("invalid_quota_snapshot")
	}
	if len(oldRaw) > 0 && json.Unmarshal(oldRaw, &current) != nil {
		return nil, false, errors.New("invalid_stored_snapshot")
	}
	if current.Binding != incoming.Binding {
		current = Snapshot{Binding: incoming.Binding}
	}
	if current.Fields == nil {
		current.Fields = map[string]Stamp{}
	}
	for name, present := range map[string]bool{"permission": current.Permission != nil, "five_hour": current.FiveHour != nil, "weekly": current.Weekly != nil, "reset_credits": current.ResetCredits != nil} {
		if _, exists := current.Fields[name]; present && !exists && !current.ObservedAt.IsZero() {
			stamp := Stamp{current.Source, current.ObservedAt, current.ReadStartedAt}
			if stamp.ReadStartedAt.IsZero() {
				stamp.ReadStartedAt = current.ObservedAt
			}
			current.Fields[name] = stamp
		}
	}
	changed := false
	stamp := Stamp{incoming.Source, incoming.ObservedAt, incoming.ReadStartedAt}
	accept := func(name string, present bool) bool {
		if !present {
			return false
		}
		previous := current.Fields[name]
		if stamp.ReadStartedAt.Before(previous.ReadStartedAt) || stamp.ReadStartedAt.Equal(previous.ReadStartedAt) && !stamp.ObservedAt.After(previous.ObservedAt) {
			return false
		}
		current.Fields[name] = stamp
		changed = true
		return true
	}
	if accept("permission", incoming.Permission != nil) {
		current.Permission = incoming.Permission
	}
	if accept("five_hour", incoming.FiveHour != nil) {
		current.FiveHour = incoming.FiveHour
	}
	if accept("weekly", incoming.Weekly != nil) {
		current.Weekly = incoming.Weekly
	}
	if accept("reset_credits", incoming.ResetCredits != nil) {
		current.ResetCredits = incoming.ResetCredits
	}
	if !changed {
		return oldRaw, false, nil
	}
	if current.ObservedAt.Before(incoming.ObservedAt) {
		current.ObservedAt = incoming.ObservedAt
		current.ReadStartedAt = incoming.ReadStartedAt
		current.Source = incoming.Source
	}
	raw, err := json.Marshal(current)
	return raw, true, err
}
