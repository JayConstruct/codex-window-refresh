package executor

// These headers identify model calls to other CPA plugins without placing
// account identities, prompts or credentials in the origin marker. CPA also
// supplies its own plugin_host_model_callback source on lifecycle callbacks.
func pluginModelHeaders(action string) map[string][]string {
	return map[string][]string{
		"Content-Type":        {"application/json"},
		"X-CPA-Plugin-ID":     {"codex-window-refresh"},
		"X-CPA-Plugin-Action": {action},
	}
}
