package core

import (
	"fmt"
	"time"
)

type ActionSource struct {
	Trigger        string    `json:"trigger"`
	ConfigPath     string    `json:"config_path,omitempty"`
	RuleID         string    `json:"rule_id,omitempty"`
	EventID        EventID   `json:"event_id,omitempty"`
	EvidenceSource string    `json:"evidence_source,omitempty"`
	EvidenceAt     time.Time `json:"evidence_at,omitempty"`
	ResetAt        time.Time `json:"reset_at,omitempty"`
	CycleID        string    `json:"cycle_id,omitempty"`
}

type HistoryEntry struct {
	Provider      string               `json:"provider,omitempty"`
	ID            uint64               `json:"id"`
	Kind          string               `json:"kind"`
	AccountID     AccountID            `json:"account_id"`
	Generation    CredentialGeneration `json:"generation"`
	OperationID   OperationID          `json:"operation_id,omitempty"`
	Sources       []ActionSource       `json:"sources"`
	StartedAt     time.Time            `json:"started_at"`
	FinishedAt    time.Time            `json:"finished_at,omitempty"`
	Status        string               `json:"status"`
	Model         string               `json:"model,omitempty"`
	Prompt        string               `json:"prompt,omitempty"`
	ErrorCode     string               `json:"error_code,omitempty"`
	CoveredEvents []EventID            `json:"covered_events,omitempty"`
}

type PromptGenerationRequest struct {
	Provider  string    `json:"provider,omitempty"`
	RequestID string    `json:"request_id"`
	AccountID AccountID `json:"account_id"`
	Model     string    `json:"model"`
	Count     int       `json:"count"`
	Target    string    `json:"target"`
}

type PromptGenerationJob struct {
	Request     PromptGenerationRequest `json:"request"`
	Generation  CredentialGeneration    `json:"generation"`
	Phase       string                  `json:"phase"`
	OperationID OperationID             `json:"operation_id,omitempty"`
	HistoryID   uint64                  `json:"history_id"`
	Candidates  []string                `json:"candidates"`
	StartedAt   time.Time               `json:"started_at"`
	FinishedAt  time.Time               `json:"finished_at,omitempty"`
	ErrorCode   string                  `json:"error_code,omitempty"`
}

// GenerationAccount is a generation-only credential, outside the Codex quota
// and automatic wake pool. It contains no tokens or raw identity metadata.
type GenerationAccount struct {
	ID           AccountID            `json:"id"`
	Provider     string               `json:"provider"`
	Generation   CredentialGeneration `json:"generation"`
	Enabled      bool                 `json:"enabled"`
	Availability string               `json:"availability"`
}

func AppendHistory(state *CoreState, entry HistoryEntry) uint64 {
	entry.ID = NextSequence(state)
	state.History = append(state.History, entry)
	return entry.ID
}

func GenerationPrompt(count int) string {
	return fmt.Sprintf("Generate exactly %d distinct harmless, short messages to request a brief greeting or acknowledgement. Return only a JSON array of strings. Each string must be single-line, at most 256 UTF-8 bytes. No personal data, secrets, role instructions or tools.", count)
}
