// Package ingest commits native event batches before acknowledging their watermarks.
package ingest

import (
	"context"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/hostlink"
	"codex-window-refresh/internal/v2/store"
)

type Link interface {
	Handshake(context.Context) (hostlink.Hello, error)
	PullEvents(context.Context, string, uint64) (events.Batch, error)
	AckEvents(context.Context, string, uint64) error
}
type Consumer struct {
	Store *store.Store
	Link  Link
}

func (c Consumer) Step(ctx context.Context) error {
	_, err := c.step(ctx)
	return err
}
func (c Consumer) step(ctx context.Context) (bool, error) {
	hello, err := c.Link.Handshake(ctx)
	if err != nil {
		return false, err
	}
	if !hello.Events || hello.HostProcess == "" {
		return false, &protocolError{}
	}
	cursor, err := c.Store.EventCursor(ctx)
	if err != nil {
		return false, err
	}
	after := uint64(0)
	if cursor.Instance == hello.Instance {
		after = cursor.Sequence
	}
	batch, err := c.Link.PullEvents(ctx, hello.Instance, after)
	if err != nil {
		return false, err
	}
	if batch.HostProcess != hello.HostProcess {
		return false, &protocolError{}
	}
	through, err := c.Store.ApplyEvents(ctx, batch)
	if err != nil {
		return false, err
	}
	// A lost acknowledgement is harmless: the next iteration reads the durable
	// cursor, and the native buffer still accepts an idempotent acknowledgement.
	return through < batch.Latest, c.Link.AckEvents(ctx, hello.Instance, through)
}

type protocolError struct{}

func (*protocolError) Error() string { return "bridge_protocol_incompatible" }
func (c Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 3*time.Second)
		more, err := c.step(step)
		cancel()
		delay := 200 * time.Millisecond
		if more && err == nil {
			delay = 0
		}
		if err != nil {
			mark, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_ = c.Store.EventDisconnected(mark, err.Error())
			stop()
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
