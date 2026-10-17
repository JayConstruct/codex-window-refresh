// Package hostlink implements the private, bounded native bridge RPC.
package hostlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/store"
)

const MaxFrame = 4 << 20

type Client struct {
	Path       string
	instance   atomic.Value
	generation atomic.Value
}
type generationCapability struct {
	instance  string
	providers []string
}
type Hello struct {
	Protocol            int      `json:"protocol_version"`
	Instance            string   `json:"bridge_instance_id"`
	ReadOnly            bool     `json:"read_only"`
	Events              bool     `json:"events_supported"`
	HostProcess         string   `json:"host_process_id"`
	PassiveQuota        bool     `json:"passive_quota_supported"`
	PromptGeneration    bool     `json:"prompt_generation_supported"`
	GenerationProviders []string `json:"prompt_generation_providers"`
}
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  struct {
		Code string `json:"code"`
	} `json:"error"`
}

func PeerOwned(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *syscall.Ucred
	var inner error
	if err = raw.Control(func(fd uintptr) {
		peer, inner = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if inner != nil || peer == nil || peer.Uid != uint32(os.Geteuid()) {
		return errors.New("socket_peer_mismatch")
	}
	return nil
}
func (c *Client) exchange(ctx context.Context, method string, request, response any) error {
	instance, _ := c.instance.Load().(string)
	return c.exchangeFor(ctx, method, request, response, instance)
}
func (c *Client) exchangeFor(ctx context.Context, method string, request, response any, instance string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		return errors.New("bridge_unavailable")
	}
	defer conn.Close()
	if err = PeerOwned(conn.(*net.UnixConn)); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	raw, err := json.Marshal(map[string]any{"protocol_version": 1, "request_id": store.ID(), "bridge_instance_id": instance, "remaining_timeout_ms": max(1, time.Until(deadline).Milliseconds()), "method": method, "payload": request})
	if err != nil || len(raw) > MaxFrame {
		return errors.New("bridge_request_invalid")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err = io.Copy(conn, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(raw))); err != nil {
		return errors.New("bridge_transport_failed")
	}
	if _, err = io.ReadFull(conn, header[:]); err != nil {
		return errors.New("bridge_transport_failed")
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrame {
		return errors.New("bridge_response_invalid")
	}
	raw = make([]byte, size)
	defer clear(raw)
	if _, err = io.ReadFull(conn, raw); err != nil {
		return errors.New("bridge_transport_failed")
	}
	var e envelope
	if json.Unmarshal(raw, &e) != nil {
		return errors.New("bridge_response_invalid")
	}
	if !e.OK {
		switch e.Error.Code {
		case "bridge_busy", "bridge_instance_changed", "method_not_allowed", "host_callback_failed", "bridge_stopping", "invalid_event_cursor",
			"generation_not_supported", "generation_request_invalid", "generation_delivery_unknown", "generation_candidates_invalid",
			"account_binding_changed", "account_not_found", "account_identity_unavailable", "account_disabled":
			return errors.New(e.Error.Code)
		}
		return errors.New("host_callback_failed")
	}
	if response != nil && json.Unmarshal(e.Result, response) != nil {
		return errors.New("bridge_response_invalid")
	}
	return ctx.Err()
}

func (c *Client) PullEvents(ctx context.Context, instance string, after uint64) (events.Batch, error) {
	var b events.Batch
	err := c.exchangeFor(ctx, "bridge.events.pull", map[string]any{"after": after, "limit": events.BatchLimit}, &b, instance)
	if err == nil && (b.Instance != instance || b.After != after || b.HostProcess == "") {
		err = errors.New("bridge_response_invalid")
	}
	return b, err
}
func (c *Client) AckEvents(ctx context.Context, instance string, through uint64) error {
	return c.exchangeFor(ctx, "bridge.events.ack", map[string]any{"through": through}, nil, instance)
}
func (c *Client) PublishBindings(ctx context.Context, instance string, accounts []store.Account) error {
	type binding struct {
		ID      string `json:"id"`
		Index   string `json:"auth_index"`
		Binding string `json:"binding"`
	}
	list := []binding{}
	for _, a := range accounts {
		if a.Binding != "" && a.AuthIndex != "" {
			list = append(list, binding{a.ID, a.AuthIndex, a.Binding})
		}
	}
	return c.exchangeFor(ctx, "bridge.bindings.replace", map[string]any{"accounts": list}, nil, instance)
}

func (c *Client) Handshake(ctx context.Context) (Hello, error) {
	c.generation.Store(generationCapability{})
	var h Hello
	var raw json.RawMessage
	err := c.exchange(ctx, "bridge.hello", struct{}{}, &raw)
	if err == nil && json.Unmarshal(raw, &h) != nil {
		err = errors.New("bridge_response_invalid")
	}
	if err == nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			err = errors.New("bridge_protocol_incompatible")
		} else if providers, present := fields["prompt_generation_providers"]; present && (len(bytes.TrimSpace(providers)) == 0 || bytes.TrimSpace(providers)[0] != '[') {
			err = errors.New("bridge_protocol_incompatible")
		}
	}
	if err == nil && (h.Protocol != 1 || h.Instance == "" || !h.ReadOnly) {
		err = errors.New("bridge_protocol_incompatible")
	}
	if err == nil {
		seen := map[string]bool{}
		for _, provider := range h.GenerationProviders {
			if !h.PromptGeneration || (provider != "codex" && provider != "antigravity") || seen[provider] {
				err = errors.New("bridge_protocol_incompatible")
				break
			}
			seen[provider] = true
		}
	}
	if err == nil {
		c.instance.Store(h.Instance)
		capability := generationCapability{}
		if h.PromptGeneration {
			capability.instance = h.Instance
			if h.GenerationProviders == nil {
				h.GenerationProviders = []string{"codex"}
			}
			capability.providers = append([]string(nil), h.GenerationProviders...)
		}
		c.generation.Store(capability)
	}
	return h, err
}
func (c *Client) SupportsPromptGeneration() bool {
	return len(c.SupportedGenerationProviders()) != 0
}
func (c *Client) SupportedGenerationProviders() []string {
	instance, _ := c.instance.Load().(string)
	capability, _ := c.generation.Load().(generationCapability)
	if instance == "" || instance != capability.instance {
		return []string{}
	}
	return append([]string{}, capability.providers...)
}
func (c *Client) Call(ctx context.Context, method string, request, response any) error {
	switch method {
	case "host.auth.list", "host.auth.get_runtime", "host.auth.get":
	case "host.model.execute_stream", "host.model.stream_read", "host.model.stream_close":
		instance, _ := c.instance.Load().(string)
		capability, _ := c.generation.Load().(generationCapability)
		if instance == "" || instance != capability.instance || len(capability.providers) == 0 {
			return errors.New("generation_not_supported")
		}
		return c.exchangeFor(ctx, method, request, response, instance)
	default:
		return errors.New("method_not_allowed")
	}
	if _, ok := c.instance.Load().(string); !ok {
		if _, err := c.Handshake(ctx); err != nil {
			return err
		}
	}
	err := c.exchange(ctx, method, request, response)
	if err != nil && err.Error() == "bridge_instance_changed" {
		if _, err = c.Handshake(ctx); err == nil {
			return c.exchange(ctx, method, request, response)
		}
	}
	return err
}
