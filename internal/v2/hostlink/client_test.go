package hostlink

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
)

type bridgeTestRequest struct {
	Method   string `json:"method"`
	Instance string `json:"bridge_instance_id"`
}

type bridgeTestServer struct {
	mu      sync.Mutex
	methods []string
	respond func(bridgeTestRequest) any
}

func newBridgeTestClient(t *testing.T, respond func(bridgeTestRequest) any) (*Client, *bridgeTestServer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("local Unix sockets unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	server := &bridgeTestServer{respond: respond}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					t.Errorf("request header: %v", err)
					return
				}
				size := binary.BigEndian.Uint32(header[:])
				if size == 0 || size > MaxFrame {
					t.Errorf("invalid request size: %d", size)
					return
				}
				raw := make([]byte, size)
				if _, err := io.ReadFull(conn, raw); err != nil {
					t.Errorf("request body: %v", err)
					return
				}
				var request bridgeTestRequest
				if json.Unmarshal(raw, &request) != nil {
					t.Error("invalid request JSON")
					return
				}
				server.mu.Lock()
				server.methods = append(server.methods, request.Method)
				server.mu.Unlock()
				raw, err = json.Marshal(server.respond(request))
				if err != nil {
					t.Errorf("response: %v", err)
					return
				}
				binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
				if _, err := conn.Write(append(header[:], raw...)); err != nil {
					t.Errorf("response write: %v", err)
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-joined })
	return &Client{Path: path}, server
}

func (server *bridgeTestServer) calls() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.methods...)
}

func bridgeHello(instance string, generation bool) any {
	return map[string]any{"ok": true, "result": map[string]any{"protocol_version": 1, "bridge_instance_id": instance, "read_only": true, "prompt_generation_supported": generation}}
}

func bridgeError(code string) any {
	return map[string]any{"ok": false, "error": map[string]string{"code": code}}
}

func TestModelCallsNeverHandshakeOrFallback(t *testing.T) {
	client := &Client{Path: "/unused/not-a-bridge"}
	for _, method := range []string{"host.model.execute_stream", "host.model.stream_read", "host.model.stream_close"} {
		if err := client.Call(context.Background(), method, struct{}{}, &struct{}{}); err == nil || err.Error() != "generation_not_supported" {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
	if client.SupportsPromptGeneration() {
		t.Fatal("uninitialized bridge advertises generation")
	}
	for _, method := range []string{"host.model.execute", "host.model.list", "host.model.stream_open", "host.other"} {
		if err := client.Call(context.Background(), method, struct{}{}, &struct{}{}); err == nil || err.Error() != "method_not_allowed" {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
}

func TestOldBridgeRejectsGenerationWithoutModelRPC(t *testing.T) {
	client, server := newBridgeTestClient(t, func(request bridgeTestRequest) any {
		if request.Method == "bridge.hello" {
			return map[string]any{"ok": true, "result": map[string]any{"protocol_version": 1, "bridge_instance_id": "old", "read_only": true}}
		}
		return map[string]any{"ok": true, "result": map[string]any{"files": []any{}}}
	})
	if err := client.Call(context.Background(), "host.auth.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if client.SupportsPromptGeneration() {
		t.Fatal("old bridge advertises generation")
	}
	for _, method := range []string{"host.model.execute_stream", "host.model.stream_read", "host.model.stream_close"} {
		if err := client.Call(context.Background(), method, struct{}{}, &struct{}{}); err == nil || err.Error() != "generation_not_supported" {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
	if !reflect.DeepEqual(server.calls(), []string{"bridge.hello", "host.auth.list"}) {
		t.Fatalf("model request escaped old bridge: %v", server.calls())
	}
}

func TestModelStreamMethodsAreSingleAttemptAndInstancePinned(t *testing.T) {
	for _, method := range []string{"host.model.execute_stream", "host.model.stream_read", "host.model.stream_close"} {
		t.Run(method, func(t *testing.T) {
			client, server := newBridgeTestClient(t, func(request bridgeTestRequest) any {
				if request.Method == "bridge.hello" {
					return bridgeHello("generation-instance", true)
				}
				if request.Instance != "generation-instance" || request.Method != method {
					t.Errorf("unpinned or unexpected request: %+v", request)
				}
				return bridgeError("bridge_instance_changed")
			})
			hello, err := client.Handshake(context.Background())
			if err != nil || !hello.ReadOnly || !hello.PromptGeneration || !client.SupportsPromptGeneration() {
				t.Fatalf("hello=%+v err=%v", hello, err)
			}
			if err := client.Call(context.Background(), method, struct{}{}, &struct{}{}); err == nil || err.Error() != "bridge_instance_changed" {
				t.Fatalf("method=%s err=%v", method, err)
			}
			if !reflect.DeepEqual(server.calls(), []string{"bridge.hello", method}) {
				t.Fatalf("model action retried or handshook: %v", server.calls())
			}
		})
	}
}

func TestAuthReadsStillRefreshChangedBridge(t *testing.T) {
	hellos := 0
	authReads := 0
	client, server := newBridgeTestClient(t, func(request bridgeTestRequest) any {
		if request.Method == "bridge.hello" {
			hellos++
			if hellos == 1 {
				return bridgeHello("first", true)
			}
			return bridgeHello("second", false)
		}
		authReads++
		if authReads == 1 {
			return bridgeError("bridge_instance_changed")
		}
		if request.Instance != "second" {
			t.Errorf("auth not repinned: %+v", request)
		}
		return map[string]any{"ok": true, "result": map[string]any{"files": []any{}}}
	})
	if err := client.Call(context.Background(), "host.auth.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if client.SupportsPromptGeneration() {
		t.Fatal("new unsupported bridge retained old generation capability")
	}
	if !reflect.DeepEqual(server.calls(), []string{"bridge.hello", "host.auth.list", "bridge.hello", "host.auth.list"}) {
		t.Fatalf("unexpected auth recovery: %v", server.calls())
	}
}

func TestGenerationErrorSanitization(t *testing.T) {
	for _, code := range []string{"generation_not_supported", "generation_request_invalid", "generation_delivery_unknown", "generation_candidates_invalid", "account_binding_changed", "account_not_found", "account_identity_unavailable", "account_disabled", "private Authorization: secret"} {
		t.Run(code, func(t *testing.T) {
			client, _ := newBridgeTestClient(t, func(request bridgeTestRequest) any {
				if request.Method == "bridge.hello" {
					return bridgeHello("supported", true)
				}
				return bridgeError(code)
			})
			if _, err := client.Handshake(context.Background()); err != nil {
				t.Fatal(err)
			}
			expected := code
			if code == "private Authorization: secret" {
				expected = "host_callback_failed"
			}
			if err := client.Call(context.Background(), "host.model.execute_stream", struct{}{}, &struct{}{}); err == nil || err.Error() != expected {
				t.Fatalf("code=%s err=%v", code, err)
			}
		})
	}
}

func TestPromptCapabilityDoesNotRelaxReadOnlyHandshake(t *testing.T) {
	client, _ := newBridgeTestClient(t, func(bridgeTestRequest) any {
		return map[string]any{"ok": true, "result": map[string]any{"protocol_version": 1, "bridge_instance_id": "unsafe", "read_only": false, "prompt_generation_supported": true}}
	})
	if _, err := client.Handshake(context.Background()); err == nil || err.Error() != "bridge_protocol_incompatible" || client.SupportsPromptGeneration() {
		t.Fatalf("err=%v supported=%v", err, client.SupportsPromptGeneration())
	}
}

func TestGenerationProviderCapabilitiesAndCopies(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		supported bool
		providers []string
		want      []string
	}{
		{"legacy-supported", true, nil, []string{"codex"}},
		{"unsupported", false, nil, []string{}},
		{"explicit-none", true, []string{}, []string{}},
		{"codex", true, []string{"codex"}, []string{"codex"}},
		{"antigravity", true, []string{"antigravity"}, []string{"antigravity"}},
		{"both", true, []string{"codex", "antigravity"}, []string{"codex", "antigravity"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			client, server := newBridgeTestClient(t, func(bridgeTestRequest) any {
				result := map[string]any{"protocol_version": 1, "bridge_instance_id": "providers", "read_only": true, "prompt_generation_supported": fixture.supported}
				if fixture.providers != nil {
					result["prompt_generation_providers"] = fixture.providers
				}
				return map[string]any{"ok": true, "result": result}
			})
			hello, err := client.Handshake(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := client.SupportedGenerationProviders(); !reflect.DeepEqual(got, fixture.want) {
				t.Fatalf("providers=%v want=%v", got, fixture.want)
			}
			if client.SupportsPromptGeneration() != (len(fixture.want) > 0) {
				t.Fatal("boolean capability disagrees with provider list")
			}
			if len(fixture.want) > 0 {
				hello.GenerationProviders[0] = "corrupted-hello"
				providers := client.SupportedGenerationProviders()
				providers[0] = "corrupted-getter"
				if !reflect.DeepEqual(client.SupportedGenerationProviders(), fixture.want) {
					t.Fatal("provider cache aliases caller-owned slices")
				}
			}
			if !reflect.DeepEqual(server.calls(), []string{"bridge.hello"}) {
				t.Fatal("capability getter performed RPC")
			}
		})
	}
}

func TestInvalidProviderHandshakeRevokesCachedGeneration(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		supported bool
		providers any
	}{
		{"unknown", true, []string{"other"}},
		{"cross-protocol", true, []string{"openai-response"}},
		{"duplicate", true, []string{"codex", "codex"}},
		{"false-with-providers", false, []string{"antigravity"}},
		{"wrong-type", true, "codex"},
		{"null", true, nil},
		{"non-string", true, []any{"codex", 1}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			hellos := 0
			client, server := newBridgeTestClient(t, func(bridgeTestRequest) any {
				hellos++
				if hellos == 1 {
					return bridgeHello("trusted", true)
				}
				return map[string]any{"ok": true, "result": map[string]any{"protocol_version": 1, "bridge_instance_id": "untrusted", "read_only": true,
					"prompt_generation_supported": fixture.supported, "prompt_generation_providers": fixture.providers}}
			})
			if _, err := client.Handshake(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Handshake(context.Background()); err == nil {
				t.Fatal("invalid providers accepted")
			}
			if client.SupportsPromptGeneration() || len(client.SupportedGenerationProviders()) != 0 {
				t.Fatal("failed handshake retained generation capability")
			}
			if instance, _ := client.instance.Load().(string); instance != "trusted" {
				t.Fatal("failed handshake repinned client")
			}
			if err := client.Call(context.Background(), "host.model.execute_stream", struct{}{}, &struct{}{}); err == nil || err.Error() != "generation_not_supported" {
				t.Fatalf("model dispatch after failed handshake: %v", err)
			}
			if !reflect.DeepEqual(server.calls(), []string{"bridge.hello", "bridge.hello"}) {
				t.Fatal("failed handshake caused model retry")
			}
		})
	}
}
