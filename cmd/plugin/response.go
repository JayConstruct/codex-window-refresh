package main

import (
	"context"
	"encoding/json"
	"errors"
)

// decodeHostResult preserves resource handles from a successful host response
// before reporting cancellation. Otherwise an opened stream could become
// unreachable and leak during the native shutdown drain.
func decodeHostResult(ctx context.Context, data []byte, response any) error {
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return errors.New("invalid host envelope")
	}
	if !envelope.OK {
		return errors.New("host RPC rejected")
	}
	if response != nil {
		if err := json.Unmarshal(envelope.Result, response); err != nil {
			return errors.New("invalid host result")
		}
	}
	return ctx.Err()
}
