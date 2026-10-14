package cpa

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeProxyTransportReusesConnectionWithFreshCredentials(t *testing.T) {
	var connections atomic.Int32
	var mu sync.Mutex
	headers := []string{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		headers = append(headers, request.Header.Get("Authorization"))
		mu.Unlock()
		if request.Header.Get("Chatgpt-Account-Id") != "real" || request.URL.Path != "/backend-api/wham/usage" {
			t.Error("reused connection lost its request identity")
		}
		_, _ = io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	token := "first"
	callback := callbackFunc(func(_ context.Context, method string, req, out any) error {
		if method != "host.auth.get" {
			t.Fatal("explicit direct proxy used host HTTP")
		}
		credential, _ := json.Marshal(map[string]string{"type": "codex", "account_id": "real", "access_token": token, "proxy_url": "direct"})
		return respond(out, authGet{AuthIndex: "auth-index", JSON: credential})
	})
	factories := 0
	reader := &NativeReader{Callback: callback, TransportForProxy: func(proxy string) (http.RoundTripper, error) {
		factories++
		roundTripper, err := nativeProxyTransport(proxy)
		if err != nil {
			return nil, err
		}
		transport := roundTripper.(*http.Transport)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local test server only.
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		return transport, nil
	}}
	defer reader.Close()
	for _, current := range []string{"first", "rotated"} {
		token = current
		if _, err := reader.Read(context.Background(), nativeTestBinding()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if connections.Load() != 1 || factories != 1 || len(headers) != 2 || headers[0] != "Bearer first" || headers[1] != "Bearer rotated" {
		t.Fatalf("connection or credentials were cached incorrectly: connections=%d factories=%d headers=%d", connections.Load(), factories, len(headers))
	}
}

type trackedQuotaTransport struct{ closed atomic.Int32 }

func (*trackedQuotaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

func (t *trackedQuotaTransport) CloseIdleConnections() { t.closed.Add(1) }

func TestProxyPoolSeparatesCredentialsAndBoundsIdleTransports(t *testing.T) {
	factories := 0
	created := []*trackedQuotaTransport{}
	r := &NativeReader{TransportForProxy: func(string) (http.RoundTripper, error) {
		factories++
		transport := &trackedQuotaTransport{}
		created = append(created, transport)
		return transport, nil
	}}
	defer r.Close()
	first, release, err := r.acquireProxyTransport("http://user:first@proxy.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	release()
	reused, release, err := r.acquireProxyTransport("http://user:first@PROXY.invalid:8080/")
	if err != nil || first != reused || factories != 1 {
		t.Fatal("equivalent proxy configurations created duplicate pools")
	}
	release()
	rotated, release, err := r.acquireProxyTransport("http://user:rotated@proxy.invalid:8080")
	if err != nil || rotated == first || factories != 2 {
		t.Fatal("rotated proxy credentials reused an authenticated connection")
	}
	release()
	for i := 0; i < maxProxyTransports; i++ {
		_, release, err := r.acquireProxyTransport(fmt.Sprintf("http://proxy-%d.invalid:8080", i))
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if len(r.proxyTransports) != maxProxyTransports || created[0].closed.Load() != 1 || created[1].closed.Load() != 1 {
		t.Fatal("proxy pool did not evict least recently used connections")
	}
	if err := r.Close(); err != nil || len(r.proxyTransports) != 0 {
		t.Fatal("pool shutdown did not release entries")
	}
	for _, transport := range created {
		if transport.closed.Load() != 1 {
			t.Fatal("idle connections were leaked or closed repeatedly")
		}
	}
	if _, _, err := r.acquireProxyTransport("direct"); err == nil {
		t.Fatal("closed reader created a new transport")
	}
}

func TestProxyPoolConcurrentReadersAndDeferredRetirement(t *testing.T) {
	var factories atomic.Int32
	transport := &trackedQuotaTransport{}
	r := &NativeReader{TransportForProxy: func(string) (http.RoundTripper, error) {
		factories.Add(1)
		return transport, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.proxyHTTP(context.Background(), "direct", http.Header{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if factories.Load() != 1 {
		t.Fatal("concurrent reads created duplicate transports")
	}
	_, release, err := r.acquireProxyTransport("direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil || transport.closed.Load() != 0 {
		t.Fatal("active transport was retired before response completion")
	}
	release()
	if transport.closed.Load() != 1 {
		t.Fatal("retired active transport leaked its idle connection")
	}
}

func TestProxyPoolExpiresIdleTransportsAndSanitizesFactoryErrors(t *testing.T) {
	created := []*trackedQuotaTransport{}
	fail := false
	r := &NativeReader{TransportForProxy: func(proxy string) (http.RoundTripper, error) {
		if fail {
			return nil, errors.New("private proxy password: " + proxy)
		}
		transport := &trackedQuotaTransport{}
		created = append(created, transport)
		return transport, nil
	}}
	defer r.Close()
	_, release, err := r.acquireProxyTransport("direct")
	if err != nil {
		t.Fatal(err)
	}
	release()
	r.proxyMu.Lock()
	r.proxyTransports["direct"].lastUsed = time.Now().Add(-proxyTransportIdleTimeout)
	r.proxyMu.Unlock()
	_, release, err = r.acquireProxyTransport("direct")
	if err != nil || len(created) != 2 || created[0].closed.Load() != 1 {
		t.Fatal("expired idle transport was retained")
	}
	release()
	fail = true
	_, err = r.proxyHTTP(context.Background(), "http://user:private@proxy.invalid:8080", http.Header{})
	if !errors.Is(err, ErrQuotaProxyInvalid) || strings.Contains(err.Error(), "private") {
		t.Fatal("factory error leaked proxy credentials")
	}
	if _, _, err := r.acquireProxyTransport("http://user:private@proxy.invalid/?token=private"); !errors.Is(err, ErrQuotaProxyInvalid) {
		t.Fatal("invalid proxy URL was passed to the factory")
	}
}

func TestProxyNormalizationPreservesCredentialsAndIPv6ZoneCase(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"HTTP://User:Password@PROXY.invalid:8080/", "http://User:Password@proxy.invalid:8080"},
		{"http://[FE80::ABCD%25ethA]:8080", "http://[fe80::abcd%25ethA]:8080"},
		{" NONE ", "direct"},
	} {
		if got, err := normalizeQuotaProxy(test.input); err != nil || got != test.want {
			t.Fatalf("proxy normalization changed routing identity: got=%q want=%q err=%v", got, test.want, err)
		}
	}
}
