// window-refresh checks local policy files without connecting to CPA.
package main

import (
	"fmt"
	"os"

	"codex-window-refresh/internal/config"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "check" {
		fmt.Fprintln(os.Stderr, "usage: window-refresh check CONFIG.yaml|CONFIG.json")
		os.Exit(2)
	}
	cfg, err := config.Load(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cfg.Activity.IdentitySource != "host_caller_scope" {
		fmt.Fprintln(os.Stderr, "CPA adapter requires activity.identity_source=host_caller_scope")
		os.Exit(1)
	}
	fmt.Printf("Configuration valid: enabled=%t timezone=%s daily_rules=%d; effective concurrency=1\n", cfg.Enabled, cfg.Timezone, len(cfg.DailyRules))
}
