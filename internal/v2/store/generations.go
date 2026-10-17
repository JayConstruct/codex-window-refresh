package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrInvalidGeneration = errors.New("invalid_prompt_generation")
var ErrGenerationAccount = errors.New("generation_account_unavailable")

type GenerationRequest struct {
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
	Count     int    `json:"count"`
}

type Generation struct {
	ID         string   `json:"job_id"`
	Phase      string   `json:"phase"`
	ErrorCode  string   `json:"error_code,omitempty"`
	Candidates []string `json:"candidates"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	InstanceID string   `json:"instance_id,omitempty"`
	Account    Account  `json:"-"`
	Model      string   `json:"-"`
	Count      int      `json:"-"`
}

func (request GenerationRequest) Validate() error {
	if !ValidID(request.AccountID) || request.Count < 1 || request.Count > 20 || len(request.Model) == 0 || len(request.Model) > 200 || !utf8.ValidString(request.Model) || strings.IndexFunc(request.Model, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) >= 0 {
		return ErrInvalidGeneration
	}
	return nil
}

func generationAccount(ctx context.Context, tx *sql.Tx, id string) (Account, error) {
	var account Account
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.auth_index,a.status,b.binding FROM accounts a JOIN account_bindings b ON b.account_id=a.id WHERE a.id=? AND a.status='available' AND a.auth_index!='' AND b.binding!='' AND NOT EXISTS(SELECT 1 FROM accounts other WHERE other.id!=a.id AND other.auth_index=a.auth_index AND other.status!='removed') AND EXISTS(SELECT 1 FROM inventory_state WHERE singleton=1 AND status='ready' AND last_success_at>=?)`, id, time.Now().UTC().Add(-90*time.Second).Format("2006-01-02T15:04:05.000000000Z")).Scan(&account.ID, &account.AuthIndex, &account.Status, &account.Binding)
	if errors.Is(err, sql.ErrNoRows) {
		return account, ErrGenerationAccount
	}
	return account, err
}

const generationColumns = "id,account_id,auth_index,binding,provider,model,count,phase,error_code,candidates,created_at,updated_at"

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var generation Generation
	var candidates string
	err := row.Scan(&generation.ID, &generation.Account.ID, &generation.Account.AuthIndex, &generation.Account.Binding, &generation.Account.Provider, &generation.Model, &generation.Count, &generation.Phase, &generation.ErrorCode, &candidates, &generation.CreatedAt, &generation.UpdatedAt)
	if err == nil {
		if generation.Account.Provider == "" {
			generation.Account.Provider = "codex"
		}
		generation.Account.Status = "available"
		err = json.Unmarshal([]byte(candidates), &generation.Candidates)
	}
	if generation.Candidates == nil {
		generation.Candidates = []string{}
	}
	return generation, err
}

func (st *Store) Generation(ctx context.Context, id string) (Generation, error) {
	generation, err := scanGeneration(st.db.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM prompt_generations WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return generation, err
}

func generationRequestHash(key string, request GenerationRequest) (string, error) {
	if request.Validate() != nil || len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(char rune) bool { return char < 33 || char > 126 }) >= 0 {
		return "", ErrInvalidGeneration
	}
	raw, _ := json.Marshal(request)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (st *Store) LookupGeneration(ctx context.Context, key string, request GenerationRequest) (Generation, bool, error) {
	hash, err := generationRequestHash(key, request)
	if err != nil {
		return Generation{}, false, err
	}
	var id, previous string
	err = st.db.QueryRowContext(ctx, "SELECT id,request_hash FROM prompt_generations WHERE idempotency_key=?", key).Scan(&id, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, false, nil
	}
	if err != nil {
		return Generation{}, false, err
	}
	if previous != hash {
		return Generation{}, true, ErrConflict
	}
	generation, err := st.Generation(ctx, id)
	return generation, true, err
}

func (st *Store) CreateGeneration(ctx context.Context, key string, request GenerationRequest) (Generation, bool, error) {
	return st.createGeneration(ctx, key, request, nil)
}

func (st *Store) CreateGenerationFor(ctx context.Context, key string, request GenerationRequest, resolved Account) (Generation, bool, error) {
	return st.createGeneration(ctx, key, request, &resolved)
}

func (st *Store) createGeneration(ctx context.Context, key string, request GenerationRequest, resolved *Account) (Generation, bool, error) {
	hash, err := generationRequestHash(key, request)
	if err != nil {
		return Generation{}, false, err
	}
	var generation Generation
	existing := false
	err = st.write(ctx, func(tx *sql.Tx) error {
		var id, previous string
		err := tx.QueryRowContext(ctx, "SELECT id,request_hash FROM prompt_generations WHERE idempotency_key=?", key).Scan(&id, &previous)
		if err == nil {
			if previous != hash {
				return ErrConflict
			}
			existing = true
			generation, err = scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM prompt_generations WHERE id=?", id))
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var total, pending int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(phase IN ('queued','running')),0) FROM prompt_generations").Scan(&total, &pending); err != nil {
			return err
		}
		if total >= 1000 || pending >= 64 {
			return ErrBusy
		}
		var account Account
		if resolved != nil {
			account = *resolved
			if account.Provider == "" {
				account.Provider = "codex"
			}
			if account.ID != request.AccountID || account.Status != "available" || account.AuthIndex == "" || account.Binding == "" || (account.Provider != "codex" && account.Provider != "antigravity") {
				return ErrGenerationAccount
			}
		}
		if resolved == nil || account.Provider == "codex" {
			inventory, err := generationAccount(ctx, tx, request.AccountID)
			if err != nil {
				return err
			}
			if resolved != nil && (inventory.AuthIndex != account.AuthIndex || inventory.Binding != account.Binding) {
				return ErrGenerationAccount
			}
			account = inventory
			account.Provider = "codex"
		}
		stamp := now()
		generation = Generation{ID: ID(), Phase: "queued", Candidates: []string{}, CreatedAt: stamp, UpdatedAt: stamp, Account: account, Model: request.Model, Count: request.Count}
		_, err = tx.ExecContext(ctx, "INSERT INTO prompt_generations(id,idempotency_key,request_hash,account_id,auth_index,binding,provider,model,count,phase,candidates,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'queued','[]',?,?)", generation.ID, key, hash, account.ID, account.AuthIndex, account.Binding, account.Provider, request.Model, request.Count, stamp, stamp)
		return err
	})
	return generation, existing, err
}

func (st *Store) RecoverGenerations(ctx context.Context) error {
	return st.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE prompt_generations SET phase='unknown',error_code='generation_interrupted',updated_at=? WHERE phase='running'", now())
		return err
	})
}

func (st *Store) ClaimGeneration(ctx context.Context) (Generation, error) {
	var generation Generation
	err := st.write(ctx, func(tx *sql.Tx) error {
		var running int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM prompt_generations WHERE phase='running'").Scan(&running); err != nil {
			return err
		}
		if running > 0 {
			return ErrBusy
		}
		var err error
		generation, err = scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM prompt_generations WHERE phase='queued' ORDER BY created_at,id LIMIT 1"))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		generation.Phase = "running"
		valid := generation.Account.Provider == "antigravity" && generation.Account.AuthIndex != "" && generation.Account.Binding != ""
		if generation.Account.Provider == "codex" {
			account, accountErr := generationAccount(ctx, tx, generation.Account.ID)
			if accountErr != nil && !errors.Is(accountErr, ErrGenerationAccount) {
				return accountErr
			}
			valid = accountErr == nil && account.AuthIndex == generation.Account.AuthIndex && account.Binding == generation.Account.Binding
		}
		if !valid {
			generation.Phase = "failed"
			generation.ErrorCode = "generation_account_unavailable"
		}
		generation.UpdatedAt = now()
		_, err = tx.ExecContext(ctx, "UPDATE prompt_generations SET phase=?,error_code=?,updated_at=? WHERE id=? AND phase='queued'", generation.Phase, generation.ErrorCode, generation.UpdatedAt, generation.ID)
		return err
	})
	return generation, err
}

func NormalizeGenerationCandidates(candidates []string, count int) ([]string, error) {
	if count < 1 || count > 20 || len(candidates) < 1 || len(candidates) > count {
		return nil, errors.New("generation_response_invalid")
	}
	result := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if !utf8.ValidString(candidate) || strings.ContainsAny(candidate, "\r\n\u0085\u2028\u2029") {
			return nil, errors.New("generation_response_invalid")
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || len(candidate) > 512 || strings.IndexFunc(candidate, unicode.IsControl) >= 0 {
			return nil, errors.New("generation_response_invalid")
		}
		if !seen[candidate] {
			result = append(result, candidate)
			seen[candidate] = true
		}
	}
	return result, nil
}

func (st *Store) FinishGeneration(ctx context.Context, id, phase, code string, candidates []string) error {
	if phase != "completed" && phase != "failed" && phase != "unknown" {
		return ErrInvalidGeneration
	}
	if phase == "completed" && code != "" {
		return ErrInvalidGeneration
	}
	if phase != "completed" {
		switch code {
		case "generation_interrupted", "generation_outcome_unknown", "generation_response_invalid", "generation_account_unavailable", "generation_request_rejected":
		default:
			return ErrInvalidGeneration
		}
		if len(candidates) > 0 {
			return ErrInvalidGeneration
		}
	}
	return st.write(ctx, func(tx *sql.Tx) error {
		generation, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM prompt_generations WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if phase == "completed" {
			candidates, err = NormalizeGenerationCandidates(candidates, generation.Count)
			if err != nil {
				return err
			}
		} else {
			candidates = []string{}
		}
		raw, _ := json.Marshal(candidates)
		previous, _ := json.Marshal(generation.Candidates)
		if generation.Phase != "running" {
			if generation.Phase == phase && generation.ErrorCode == code && string(previous) == string(raw) {
				return nil
			}
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, "UPDATE prompt_generations SET phase=?,error_code=?,candidates=?,updated_at=? WHERE id=? AND phase='running'", phase, code, string(raw), now(), id)
		return err
	})
}
