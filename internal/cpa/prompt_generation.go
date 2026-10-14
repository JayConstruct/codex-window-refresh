package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type generationText struct {
	deltas    string
	completed string
	tooLarge  bool
}

func (text *generationText) accept(raw []byte) {
	var event struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response struct {
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if event.Type == "response.output_text.delta" && !text.tooLarge {
		if len(text.deltas)+len(event.Delta) > 16<<10 {
			text.tooLarge = true
		} else {
			text.deltas += event.Delta
		}
	}
	if event.Type == "response.completed" {
		for _, output := range event.Response.Output {
			for _, content := range output.Content {
				if content.Type != "output_text" {
					continue
				}
				if len(text.completed)+len(content.Text) > 16<<10 {
					text.tooLarge = true
					return
				}
				text.completed += content.Text
			}
		}
	}
}

func NormalizePromptCandidates(text string, count int) ([]string, error) {
	if len(text) > 16<<10 || count < 5 || count > 50 {
		return nil, errors.New("prompt_candidates_invalid")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json\n") && strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "```"))
	}
	var values []string
	if json.Unmarshal([]byte(text), &values) != nil || len(values) > 50 {
		return nil, errors.New("prompt_candidates_invalid")
	}
	unique, seen := []string{}, map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		unique, seen[value] = append(unique, value), true
	}
	if len(unique) != count || config.ValidatePromptPool(unique, true) != nil {
		return nil, errors.New("prompt_candidates_invalid")
	}
	return unique, nil
}

func (h *Host) GeneratePrompts(ctx context.Context, req core.ExactRequest, count int) (core.SendResult, []string, error) {
	text := &generationText{}
	result, err := h.sendExact(ctx, req, text)
	if err != nil || result.Disposition != core.SendCompleted {
		return result, nil, err
	}
	if text.tooLarge {
		return result, nil, errors.New("prompt_candidates_invalid")
	}
	output := text.completed
	if output == "" {
		output = text.deltas
	}
	values, err := NormalizePromptCandidates(output, count)
	return result, values, err
}
