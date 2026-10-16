// Package hostlink implements the private, bounded, read-only native bridge RPC.
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
	Path     string
	instance atomic.Value
}
type Hello struct {
	Protocol     int    `json:"protocol_version"`
	Instance     string `json:"bridge_instance_id"`
	ReadOnly     bool   `json:"read_only"`
	Events       bool   `json:"events_supported"`
	HostProcess  string `json:"host_process_id"`
	PassiveQuota bool   `json:"passive_quota_supported"`
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
		case "bridge_busy", "bridge_instance_changed", "method_not_allowed", "host_callback_failed", "bridge_stopping", "invalid_event_cursor":
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
	var h Hello
	err := c.exchange(ctx, "bridge.hello", struct{}{}, &h)
	if err == nil && (h.Protocol != 1 || h.Instance == "" || !h.ReadOnly) {
		err = errors.New("bridge_protocol_incompatible")
	}
	if err == nil {
		c.instance.Store(h.Instance)
	}
	return h, err
}
func (c *Client) Call(ctx context.Context, method string, request, response any) error {
	switch method {
	case "host.auth.list", "host.auth.get_runtime", "host.auth.get":
	default:
		return errors.New("method_not_allowed")
	}
	if _, ok := c.instance.Load().(string); !ok {
		if _, err := c.Handshake(ctx); err != nil {
			return err
		}
	}
	err := c.exchange(ctx, method, request, response)
	// Every supported call is read-only. A new native instance requires a fresh
	// handshake; no model or stream action is ever replayed here.
	if err != nil && err.Error() == "bridge_instance_changed" {
		if _, err = c.Handshake(ctx); err == nil {
			return c.exchange(ctx, method, request, response)
		}
	}
	return err
}
