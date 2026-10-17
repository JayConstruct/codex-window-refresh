package quota

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func normalized(t *testing.T, raw, source string, at, start time.Time) []byte {
	t.Helper()
	s, e := Normalize([]byte(raw), nil, "binding", source, at, start)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(s)
	return b
}
func TestPartialEvidenceAndSlowQueryKeepFieldBoundaries(t *testing.T) {
	at := time.Now().UTC()
	full := `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"window_minutes":300,"reset_after_seconds":3000},"secondary_window":{"used_percent":30,"window_minutes":10080,"reset_after_seconds":30000}}}`
	initial := normalized(t, full, "cpa_wham_usage", at, at.Add(-time.Second))
	partial := normalized(t, `{"rate_limits":{"primary":{"used_percent":40,"window_minutes":300,"reset_after_seconds":2900}}}`, "cpa_websocket_quota", at.Add(time.Second), at.Add(time.Second))
	merged, changed, err := Merge(initial, partial)
	if err != nil || !changed {
		t.Fatal(err)
	}
	// This query finishes later, but began before the execution event.
	slow := normalized(t, full, "cpa_wham_usage", at.Add(2*time.Second), at)
	final, _, err := Merge(merged, slow)
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if json.Unmarshal(final, &s) != nil || s.FiveHour.Remaining.Value != 60 || s.Weekly.Remaining.Value != 70 || s.Fields["weekly"].Source != "cpa_wham_usage" || s.Fields["five_hour"].Source != "cpa_websocket_quota" {
		t.Fatal(string(final))
	}
	if s.Fields["weekly"].ObservedAt.Equal(s.Fields["five_hour"].ObservedAt) {
		t.Fatal("absent field gained freshness")
	}
	if s.FiveHour.ResetAfterSeconds == nil || *s.FiveHour.ResetAfterSeconds != 2900 || s.Weekly.ResetAfterSeconds == nil || *s.Weekly.ResetAfterSeconds != 30000 {
		t.Fatal("reported countdown did not survive normalization and field merge")
	}
	repeated, changed, err := Merge(final, partial)
	if err != nil || changed || string(repeated) != string(final) {
		t.Fatal("replayed event changed evidence")
	}
}
func TestMissingFieldsCanArriveOutOfOrderAndReplacedBindingDropsOldFields(t *testing.T) {
	at := time.Now().UTC()
	recent := normalized(t, `{"rate_limit":{"allowed":true}}`, "cpa_response_headers", at, at)
	earlier := normalized(t, `{"rate_limit":{"primary":{"used_percent":10,"window_minutes":300,"reset_after_seconds":3000}}}`, "cpa_wham_usage", at.Add(-time.Second), at.Add(-2*time.Second))
	merged, changed, err := Merge(recent, earlier)
	if err != nil || !changed {
		t.Fatal(err)
	}
	var incoming Snapshot
	_ = json.Unmarshal(recent, &incoming)
	incoming.Binding = "replacement"
	raw, _ := json.Marshal(incoming)
	replaced, _, err := Merge(merged, raw)
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	_ = json.Unmarshal(replaced, &s)
	if s.FiveHour != nil {
		t.Fatal("replacement inherited window")
	}
}
func TestHeadersRejectConflictsAndNormalizeOnlyAllowedFields(t *testing.T) {
	at := time.Now().UTC()
	h := http.Header{"X-Codex-Allowed": {"true"}, "Authorization": {"private"}}
	s, err := Normalize(nil, h, "binding", "cpa_response_headers", at, at)
	if err != nil || s.Permission == nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if _, ok := decoded["Authorization"]; ok {
		t.Fatal("credentials persisted")
	}
	h["x-codex-allowed"] = []string{"false"}
	if _, err = Normalize(nil, h, "binding", "cpa_response_headers", at, at); err == nil {
		t.Fatal("ambiguous headers accepted")
	}
}
