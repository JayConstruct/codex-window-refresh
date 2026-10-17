package policy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestModelDefaultsAndNormalization(t *testing.T) {
	config := DefaultModel()
	if err := config.Validate(); err != nil || config.DefaultModel != "" || config.DefaultPrompt != "你好，请用一句简短的中文问候回复。" || config.PromptMode != "fixed" || config.Phrases == nil {
		t.Fatal(config, err)
	}
	config.Phrases = nil
	if config.Normalize().Phrases == nil {
		t.Fatal("nil phrases were not normalized")
	}
	config.Phrases = []string{"  第一条  ", "第二条"}
	normalized := config.Normalize()
	if normalized.Phrases[0] != "第一条" || config.Phrases[0] != "  第一条  " {
		t.Fatal("normalization must trim a copied slice")
	}
}

func TestModelValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ModelConfig)
	}{
		{"empty default prompt", func(config *ModelConfig) { config.DefaultPrompt = " \n\t" }},
		{"invalid default UTF8", func(config *ModelConfig) { config.DefaultPrompt = string([]byte{0xff}) }},
		{"long default prompt", func(config *ModelConfig) { config.DefaultPrompt = strings.Repeat("x", 8193) }},
		{"long override prompt", func(config *ModelConfig) { config.FiveHourPrompt = strings.Repeat("x", 8193) }},
		{"invalid weekly UTF8", func(config *ModelConfig) { config.WeeklyPrompt = string([]byte{0xff}) }},
		{"default prompt NUL", func(config *ModelConfig) { config.DefaultPrompt = "hello\x00world" }},
		{"override prompt control", func(config *ModelConfig) { config.FiveHourPrompt = "hello\x1bworld" }},
		{"long model", func(config *ModelConfig) { config.DefaultModel = strings.Repeat("x", 201) }},
		{"model space", func(config *ModelConfig) { config.DefaultModel = "model name" }},
		{"model unicode space", func(config *ModelConfig) { config.FiveHourModel = "model\u3000name" }},
		{"model control", func(config *ModelConfig) { config.WeeklyModel = "model\x00name" }},
		{"model invalid UTF8", func(config *ModelConfig) { config.WeeklyModel = string([]byte{0xff}) }},
		{"unknown mode", func(config *ModelConfig) { config.PromptMode = "generated" }},
		{"random empty", func(config *ModelConfig) { config.PromptMode = "random" }},
		{"empty phrase", func(config *ModelConfig) { config.Phrases = []string{"   "} }},
		{"multiline phrase", func(config *ModelConfig) { config.Phrases = []string{"hello\nworld"} }},
		{"unicode multiline phrase", func(config *ModelConfig) { config.Phrases = []string{"hello\u2028world"} }},
		{"duplicate trimmed phrases", func(config *ModelConfig) { config.Phrases = []string{"hello", " hello "} }},
		{"long phrase bytes", func(config *ModelConfig) { config.Phrases = []string{strings.Repeat("中", 171)} }},
		{"invalid phrase UTF8", func(config *ModelConfig) { config.Phrases = []string{string([]byte{0xff})} }},
		{"too many phrases", func(config *ModelConfig) {
			for index := 0; index < 201; index++ {
				config.Phrases = append(config.Phrases, fmt.Sprint(index))
			}
		}},
		{"total bytes", func(config *ModelConfig) {
			for index := 0; index < 129; index++ {
				config.Phrases = append(config.Phrases, fmt.Sprintf("%03d", index)+strings.Repeat("x", 509))
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultModel()
			test.change(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalidModelSettings) {
				t.Fatal("invalid input accepted", err)
			}
		})
	}
	config := DefaultModel()
	config.DefaultModel = strings.Repeat("x", 200)
	config.DefaultPrompt = strings.Repeat("x", 8192)
	config.FiveHourPrompt = strings.Repeat("x", 8192)
	config.WeeklyPrompt = strings.Repeat("x", 8192)
	config.PromptMode = "random"
	config.Phrases = []string{strings.Repeat("x", 512)}
	if err := config.Validate(); err != nil {
		t.Fatal("valid boundary rejected", err)
	}
	config.DefaultPrompt = "合法的\r\n多行\t提示"
	if err := config.Validate(); err != nil {
		t.Fatal("valid multiline prompt rejected", err)
	}
}

func TestModelTextBudgetCountsUTF8BytesBeforePhraseTrimming(t *testing.T) {
	config := DefaultModel()
	config.DefaultPrompt = "x"
	for index := 0; index < 127; index++ {
		config.Phrases = append(config.Phrases, fmt.Sprintf("%03d", index)+strings.Repeat("x", 509))
	}
	config.Phrases = append(config.Phrases, "last-"+strings.Repeat("x", 501))
	if err := config.Validate(); err != nil {
		t.Fatal("65536-byte configuration rejected", err)
	}
	config.Phrases[len(config.Phrases)-1] += " "
	if err := config.Validate(); !errors.Is(err, ErrInvalidModelSettings) {
		t.Fatal("trimmed whitespace bypassed input budget", err)
	}
}
