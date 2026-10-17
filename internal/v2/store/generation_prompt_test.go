package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGenerationPromptSnapshotIdempotenceAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	storage, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { storage.Close() }()
	account := Account{ID: "antig", AuthIndex: "index", Binding: "binding", Provider: "antigravity", Status: "available"}
	request := GenerationRequest{AccountID: account.ID, Model: "fixture-model", Count: 2, Prompt: "生成中文问候。\n不要提问。"}
	accepted, existing, err := storage.CreateGenerationFor(ctx, "prompt-key", request, account)
	if err != nil || existing || accepted.Prompt != request.Prompt {
		t.Fatal(accepted, existing, err)
	}
	raw, err := json.Marshal(accepted)
	if err != nil || strings.Contains(string(raw), "prompt") || strings.Contains(string(raw), request.Prompt) {
		t.Fatal("private generation parameters leaked", string(raw), err)
	}
	changed := request
	changed.Prompt = "另一种提示词"
	if _, _, err = storage.LookupGeneration(ctx, "prompt-key", changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed prompt did not conflict", err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimGeneration(ctx)
	if err != nil || claimed.ID != accepted.ID || claimed.Prompt != request.Prompt {
		t.Fatal(claimed, err)
	}
	if err = storage.RecoverGenerations(ctx); err != nil {
		t.Fatal(err)
	}
	replayed, existing, err := storage.LookupGeneration(ctx, "prompt-key", request)
	if err != nil || !existing || replayed.ID != accepted.ID || replayed.Phase != "unknown" || replayed.Prompt != request.Prompt {
		t.Fatal(replayed, existing, err)
	}
	if _, err = storage.ClaimGeneration(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown prompt generation was replayed", err)
	}
}

func TestGenerationPromptPreservesLegacyRequestHashAndValidates(t *testing.T) {
	request := GenerationRequest{AccountID: "account", Model: "model", Count: 1}
	legacy := sha256.Sum256([]byte(`{"account_id":"account","model":"model","count":1}`))
	hash, err := generationRequestHash("key", request)
	if err != nil || hash != hex.EncodeToString(legacy[:]) {
		t.Fatal("legacy hash changed", hash, err)
	}
	for _, prompt := range []string{string([]byte{0xff}), "nul\x00", strings.Repeat("x", 8193)} {
		request.Prompt = prompt
		if request.Validate() == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
	request.Prompt = strings.Repeat("x", 8192)
	if err = request.Validate(); err != nil {
		t.Fatal(err)
	}
}
