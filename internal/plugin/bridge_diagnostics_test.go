package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCapabilitiesDiagnoseMissingProcessKeyWithoutExposingSecrets(t *testing.T) {
	s, _, _, cb := setupService(t)
	const env = "CPA_WINDOW_REFRESH_DIAGNOSTIC_TEST_KEY"
	t.Setenv(env, "")
	s.wrapper.ManagementURL = "http://127.0.0.1:8317/v0/management"
	s.wrapper.ManagementKeyEnv = env
	check := func(expected string) {
		t.Helper()
		r := managementCall(t, s, "GET", ManagementPrefix+"/capabilities")
		var body struct {
			QuotaBridge quotaBridgeDiagnostic `json:"quota_bridge"`
		}
		if r.StatusCode != 200 || json.Unmarshal(r.Body, &body) != nil || body.QuotaBridge.Status != expected || body.QuotaBridge.ManagementKeyEnv != env {
			t.Fatalf("wrong bridge diagnosis: %+v", r)
		}
		if strings.Contains(string(r.Body), "sentinel-private-key") || strings.Contains(string(r.Body), s.wrapper.ManagementURL) || cb.calls != 0 {
			t.Fatal("diagnostics leaked configuration secrets or contacted the host")
		}
	}
	check("missing_management_key")
	t.Setenv(env, "sentinel-private-key")
	check("configured")
	s.wrapper.ManagementURL = "http://user:sentinel-private-key@localhost"
	check("invalid_management_url")
}
