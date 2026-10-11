package main

import (
	"context"
	"errors"
	"testing"
)

func TestCanceledHostResponseRetainsCleanupHandle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var result struct {
		StreamID string `json:"stream_id"`
	}
	err := decodeHostResult(ctx, []byte(`{"ok":true,"result":{"stream_id":"cleanup-required"}}`), &result)
	if !errors.Is(err, context.Canceled) || result.StreamID != "cleanup-required" {
		t.Fatalf("cancellation lost cleanup handle: %+v %v", result, err)
	}
}
func TestHostFailureDoesNotExposeUpstreamError(t *testing.T) {
	err := decodeHostResult(context.Background(), []byte(`{"ok":false,"error":{"message":"SECRET"}}`), nil)
	if err == nil || err.Error() != "host RPC rejected" {
		t.Fatalf("host error leaked: %v", err)
	}
}
