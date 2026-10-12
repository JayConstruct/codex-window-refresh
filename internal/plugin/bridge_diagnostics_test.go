package plugin

import (
	"context"
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
	s.wrapper.QuotaTransport = "management"
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

func TestDefaultHostQuotaTransportNeedsNoManagementEnvironment(t *testing.T) {
	s, _, _, cb := setupService(t)
	t.Setenv("CPA_MANAGEMENT_KEY", "")
	// Old installation fields remain optional and cannot disable host queries.
	w := WrapperConfig{ManagementURL: "http://127.0.0.1:8317/v0/management", ManagementKeyEnv: "CPA_MANAGEMENT_KEY"}
	h, err := New(cb, Options{}).options.HostFactory(cb, w)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := h.Capabilities(context.Background())
	if err != nil || !caps.QuotaAdapterReady || cb.calls != 0 {
		t.Fatal("default host reader depended on management configuration or performed network I/O")
	}
	s.wrapper = w
	r := managementCall(t, s, "GET", ManagementPrefix+"/capabilities")
	var body struct {
		QuotaBridge quotaBridgeDiagnostic `json:"quota_bridge"`
	}
	if json.Unmarshal(r.Body, &body) != nil || body.QuotaBridge.Mode != "host" || body.QuotaBridge.Status != "host_callback" || body.QuotaBridge.ManagementKeyEnv != "" {
		t.Fatal("host diagnosis requested a management key")
	}
}

func TestQuotaTransportWrapperValidation(t *testing.T) {
	base := "config_file: /tmp/policy.yaml\nstate_dir: /tmp/window-state\n"
	for _, tc := range []struct {
		yaml, mode string
		valid      bool
	}{
		{"", "host", true},
		{"quota_transport: host\nmanagement_url: http://localhost:8317\nmanagement_key_env: NOT_SET\n", "host", true},
		{"quota_transport: management\nmanagement_url: http://localhost:8317\nmanagement_key_env: KEY\n", "management", true},
		{"quota_transport: management\n", "", false},
		{"quota_transport: invalid\n", "", false},
	} {
		wrapper, err := parseWrapper(lifecyclePayload(t, base+tc.yaml, SchemaVersion))
		if (err == nil) != tc.valid || (tc.valid && wrapper.quotaTransport() != tc.mode) {
			t.Fatal("quota transport validation or default changed")
		}
	}
}
