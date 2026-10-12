package cpa

import "testing"

func TestBridgeConfigurationDiagnosisMatchesReadinessWithoutNetwork(t *testing.T) {
	for _, tc := range []struct {
		name, url, env, key, status string
	}{
		{"missing address", "", "KEY", "secret", "missing_management_url"},
		{"missing variable name", "http://127.0.0.1:8317", "", "secret", "missing_management_key_env"},
		{"missing process key", "http://127.0.0.1:8317", "KEY", "", "missing_management_key"},
		{"invalid scheme", "file:///secret", "KEY", "secret", "invalid_management_url"},
		{"invalid path", "http://127.0.0.1:8317/wrong", "KEY", "secret", "invalid_management_url"},
		{"embedded credentials", "http://user:secret@127.0.0.1:8317", "KEY", "secret", "invalid_management_url"},
		{"query string", "http://127.0.0.1:8317?secret=hidden", "KEY", "secret", "invalid_management_url"},
		{"v0 configured", "http://127.0.0.1:8317/v0/management", "KEY", "secret", "configured"},
		{"v8 configured", "https://localhost/v8/management/requests/api-call", "KEY", "secret", "configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &BridgeReader{ManagementURL: tc.url, ManagementKeyEnv: tc.env, Getenv: func(string) string { return tc.key }}
			if status := reader.ConfigurationStatus(); status != tc.status || reader.Available() != (status == "configured") {
				t.Fatalf("wrong readiness diagnosis: %s", status)
			}
		})
	}
}
