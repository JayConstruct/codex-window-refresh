package config

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"codex-window-refresh/internal/core"
)

func DefaultPromptPool() []string {
	return []string{"Reply OK.", "Say hello briefly.", "Respond with Ready.", "Give a short greeting.", "Reply with one friendly word."}
}

func ValidatePromptPool(values []string, required bool) error {
	if len(values) > 50 || (required && len(values) == 0) {
		return fmt.Errorf("prompt pool must contain %s", map[bool]string{true: "1 to 50 entries", false: "at most 50 entries"}[required])
	}
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 256 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("each prompt must be nonempty valid UTF-8, single-line and at most 256 bytes")
		}
		key := strings.TrimSpace(value)
		if seen[key] {
			return fmt.Errorf("duplicate prompt")
		}
		seen[key] = true
	}
	return nil
}

func (settings Config) WakePromptPool(reason core.Reason) []string {
	pool := settings.Execution.PromptPool
	switch reason {
	case core.ReasonWeekly:
		if len(settings.ResetRules.Weekly.PromptPool) > 0 {
			pool = settings.ResetRules.Weekly.PromptPool
		}
	case core.ReasonFiveHour, core.ReasonDaily:
		if len(settings.ResetRules.FiveHour.PromptPool) > 0 {
			pool = settings.ResetRules.FiveHour.PromptPool
		}
	}
	return append([]string{}, pool...)
}
