package policy

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrInvalidModelSettings = errors.New("invalid_model_settings")

const DefaultGenerationPrompt = "生成简短、自然、适合日常问候的中文短句，避免重复。"

type ModelConfig struct {
	DefaultModel     string   `json:"default_model"`
	DefaultPrompt    string   `json:"default_prompt"`
	FiveHourModel    string   `json:"five_hour_model"`
	FiveHourPrompt   string   `json:"five_hour_prompt"`
	WeeklyModel      string   `json:"weekly_model"`
	WeeklyPrompt     string   `json:"weekly_prompt"`
	GenerationPrompt string   `json:"generation_prompt"`
	PromptMode       string   `json:"prompt_mode"`
	Phrases          []string `json:"phrases"`
}

func DefaultModel() ModelConfig {
	return ModelConfig{DefaultPrompt: "你好，请用一句简短的中文问候回复。", GenerationPrompt: DefaultGenerationPrompt, PromptMode: "fixed", Phrases: []string{}}
}

func ValidGenerationPrompt(prompt string) bool {
	return utf8.ValidString(prompt) && len(prompt) <= 8192 && !strings.ContainsFunc(prompt, func(value rune) bool {
		return unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t'
	})
}

func (config ModelConfig) Normalize() ModelConfig {
	phrases := make([]string, len(config.Phrases))
	for index, phrase := range config.Phrases {
		phrases[index] = strings.TrimSpace(phrase)
	}
	config.Phrases = phrases
	return config
}

func (config ModelConfig) Validate() error {
	total := len(config.PromptMode)
	for _, model := range []string{config.DefaultModel, config.FiveHourModel, config.WeeklyModel} {
		if !utf8.ValidString(model) || len(model) > 200 || strings.ContainsFunc(model, func(value rune) bool { return unicode.IsSpace(value) || unicode.IsControl(value) }) {
			return ErrInvalidModelSettings
		}
		total += len(model)
	}
	for _, prompt := range []string{config.DefaultPrompt, config.FiveHourPrompt, config.WeeklyPrompt, config.GenerationPrompt} {
		if !utf8.ValidString(prompt) || len(prompt) > 8192 || strings.ContainsFunc(prompt, func(value rune) bool {
			return unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t'
		}) {
			return ErrInvalidModelSettings
		}
		total += len(prompt)
	}
	if strings.TrimSpace(config.DefaultPrompt) == "" || config.PromptMode != "fixed" && config.PromptMode != "random" || len(config.Phrases) > 200 || config.PromptMode == "random" && len(config.Phrases) == 0 {
		return ErrInvalidModelSettings
	}
	seen := make(map[string]bool, len(config.Phrases))
	for _, phrase := range config.Phrases {
		total += len(phrase)
		trimmed := strings.TrimSpace(phrase)
		if !utf8.ValidString(phrase) || trimmed == "" || len(trimmed) > 512 || seen[trimmed] || strings.ContainsFunc(trimmed, func(value rune) bool { return unicode.IsControl(value) || value == '\u2028' || value == '\u2029' }) {
			return ErrInvalidModelSettings
		}
		seen[trimmed] = true
	}
	if total > 65536 {
		return ErrInvalidModelSettings
	}
	return nil
}
