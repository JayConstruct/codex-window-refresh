package executor

// Earned resets use the documented Codex App Server account protocol. This
// adapter never opens a model thread and never writes CPA tokens to disk.
import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

type resetRPC struct {
	input  io.WriteCloser
	output *bufio.Scanner
	next   int
}

func (r *resetRPC) call(method string, params any, result any) error {
	r.next++
	id := r.next
	if json.NewEncoder(r.input).Encode(map[string]any{"id": id, "method": method, "params": params}) != nil {
		return errors.New("reset_transport_failed")
	}
	for n := 0; n < 256 && r.output.Scan(); n++ {
		var response struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(r.output.Bytes(), &response) != nil {
			return errors.New("reset_protocol_invalid")
		}
		if response.Method != "" {
			if response.ID != 0 {
				_ = json.NewEncoder(r.input).Encode(map[string]any{"id": response.ID, "error": map[string]any{"code": -32000, "message": "host token refresh unavailable"}})
			}
			continue
		}
		if response.ID != id {
			continue
		}
		if len(response.Error) > 0 && string(response.Error) != "null" {
			return errors.New("reset_account_rpc_failed")
		}
		if len(response.Result) == 0 || json.Unmarshal(response.Result, result) != nil {
			return errors.New("reset_protocol_invalid")
		}
		return nil
	}
	return errors.New("reset_transport_failed")
}
func (c *CPA) resetAccountRPC(ctx context.Context, a store.Account, method string, params, result any) error {
	ctx, timeoutCancel := context.WithTimeout(ctx, 45*time.Second)
	defer timeoutCancel()
	cred, err := c.resetBinding(ctx, a)
	if err != nil {
		return err
	}
	if !validQuotaToken(cred.Token) || a.Binding == "" || a.Binding != bindingID(a, cred.Upstream) {
		return errors.New("account_binding_changed")
	}
	proxy, err := accountQuotaProxy(cred)
	if err != nil {
		return err
	}
	if proxy == "" {
		proxy, err = accountQuotaProxy(credential{Proxy: c.ResetProxy})
		if err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp("", "cwr-reset-")
	if err != nil {
		return errors.New("reset_runtime_unavailable")
	}
	defer os.RemoveAll(dir)
	binary := c.CodexBinary
	if binary == "" {
		binary = "codex"
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, binary, "app-server", "--listen", "stdio://", "-c", `cli_auth_credentials_store="ephemeral"`)
	cmd.Dir = dir
	// Do not inherit another account's home, API key, endpoint, or tracing setup.
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		switch strings.ToUpper(key) {
		case "CODEX_HOME", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CHATGPT_BASE_URL", "CODEX_API_KEY":
			continue
		}
		if strings.HasPrefix(key, "OTEL_") {
			continue
		}
		if proxy != "" {
			switch strings.ToUpper(key) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
				continue
			}
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+dir)
	if proxy != "" && proxy != "direct" {
		cmd.Env = append(cmd.Env, "HTTPS_PROXY="+proxy, "HTTP_PROXY="+proxy, "ALL_PROXY="+proxy)
	}
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		return errors.New("reset_runtime_unavailable")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("reset_runtime_unavailable")
	}
	if cmd.Start() != nil {
		return errors.New("reset_codex_unavailable")
	}
	defer func() { input.Close(); cancel(); _ = cmd.Wait() }()
	rpc := resetRPC{input: input, output: bufio.NewScanner(output)}
	rpc.output.Buffer(make([]byte, 4096), 1<<20)
	var ack map[string]any
	if err = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex-window-refresh", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, &ack); err != nil {
		return err
	}
	if json.NewEncoder(input).Encode(map[string]any{"method": "initialized", "params": map[string]any{}}) != nil {
		return errors.New("reset_transport_failed")
	}
	if err = rpc.call("account/login/start", map[string]string{"type": "chatgptAuthTokens", "accessToken": cred.Token, "chatgptAccountId": cred.Upstream}, &ack); err != nil {
		return err
	}
	if ack["type"] != "chatgptAuthTokens" {
		return errors.New("reset_protocol_invalid")
	}
	// Read once more immediately before a possible consume, after process startup.
	after, err := c.resetBinding(ctx, a)
	if err != nil || bindingID(a, after.Upstream) != a.Binding {
		return errors.New("account_binding_changed")
	}
	if err = rpc.call(method, params, result); err != nil {
		return err
	}
	after, err = c.resetBinding(ctx, a)
	if err != nil || bindingID(a, after.Upstream) != a.Binding {
		return errors.New("account_binding_changed")
	}
	return nil
}
func (c *CPA) ReadResetCredits(ctx context.Context, a store.Account) (*policy.ResetCredits, error) {
	var response struct {
		Credits *policy.ResetCredits `json:"rateLimitResetCredits"`
	}
	if err := c.resetAccountRPC(ctx, a, "account/rateLimits/read", map[string]any{}, &response); err != nil {
		return nil, err
	}
	if response.Credits != nil {
		if response.Credits.AvailableCount < 0 || response.Credits.AvailableCount > 100000 || len(response.Credits.Credits) > 1000 {
			return nil, errors.New("reset_protocol_invalid")
		}
		for _, card := range response.Credits.Credits {
			if !policy.RoutingLabel(card.ID) {
				return nil, errors.New("reset_protocol_invalid")
			}
		}
	}
	return response.Credits, nil
}
func (c *CPA) ConsumeResetCredit(ctx context.Context, a store.Account, creditID, key string) (string, error) {
	if !policy.RoutingLabel(creditID) || !policy.RoutingLabel(key) {
		return "", errors.New("reset_request_invalid")
	}
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err := c.resetAccountRPC(ctx, a, "account/rateLimitResetCredit/consume", map[string]string{"creditId": creditID, "idempotencyKey": key}, &response); err != nil {
		return "", err
	}
	switch response.Outcome {
	case "reset", "alreadyRedeemed", "nothingToReset", "noCredit":
		return response.Outcome, nil
	}
	return "", errors.New("reset_protocol_invalid")
}

func (c *CPA) PublishRouting(ctx context.Context, snapshot store.RoutingSnapshot) error {
	return c.Host.Call(ctx, "bridge.scheduler.replace", snapshot, nil)
}

// Quota exhaustion may set CPA's unavailable flag. Reading/redeeming a reset
// must remain possible for that account; manual disable is always respected.
func (c *CPA) resetBinding(ctx context.Context, a store.Account) (credential, error) {
	cred, err := c.readBindingFor(ctx, a, false)
	if err != nil {
		return credential{}, err
	}
	if cred.status == "disabled" {
		return credential{}, errors.New("account_disabled")
	}
	return cred, nil
}

// Called only after an acknowledged upstream redemption and fresh allowed quota.
// This clears CPA's local cooldown; it never resets upstream quota itself.
func (c *CPA) ClearResetCooldown(ctx context.Context, a store.Account) error {
	cred, err := c.resetBinding(ctx, a)
	if err != nil {
		return err
	}
	if a.Binding == "" || bindingID(a, cred.Upstream) != a.Binding {
		return errors.New("account_binding_changed")
	}
	return c.Host.Call(ctx, "host.routing.reset_cooldown", map[string]string{"auth_index": a.AuthIndex}, nil)
}
