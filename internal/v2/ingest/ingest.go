// Package ingest commits native event batches before acknowledging their watermarks.
package ingest

import (
	"context"
	"sync"
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
	Store     *store.Store
	Link      Link
	Inventory interface {
		Accounts(context.Context) ([]store.Account, error)
		Identity(context.Context, store.Account) (store.Account, error)
	}
	lastInventory     time.Time
	inventoryInstance string
	syncOnce          sync.Once
	syncRequests      chan struct{}
}

func (c *Consumer) inventoryRequests() chan struct{} {
	c.syncOnce.Do(func() { c.syncRequests = make(chan struct{}, 1) })
	return c.syncRequests
}

func (c *Consumer) RequestInventorySync() bool {
	if c.Inventory == nil || c.Link == nil || c.Store == nil {
		return false
	}
	if _, ok := c.Link.(interface {
		PublishBindings(context.Context, string, []store.Account) error
	}); !ok {
		return false
	}
	select {
	case c.inventoryRequests() <- struct{}{}:
	default:
	}
	return true
}

func (c *Consumer) Step(ctx context.Context) error {
	_, err := c.step(ctx)
	return err
}
func (c *Consumer) step(ctx context.Context) (bool, error) {
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
func (c *Consumer) Run(ctx context.Context) {
	var inventory sync.WaitGroup
	if c.Inventory != nil {
		inventory.Add(1)
		go func() { defer inventory.Done(); c.inventoryLoop(ctx) }()
	}
	defer inventory.Wait()
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

func (c *Consumer) inventoryLoop(ctx context.Context) {
	requests := c.inventoryRequests()
	requested := false
	for ctx.Err() == nil {
		select {
		case <-requests:
			requested = true
		default:
		}
		refresh, cancel := context.WithTimeout(ctx, 60*time.Second)
		hello, err := c.Link.Handshake(refresh)
		delay := time.Second
		if err == nil && !hello.PassiveQuota {
			err = &protocolError{}
		}
		if err == nil && (requested || hello.Instance != c.inventoryInstance || time.Since(c.lastInventory) >= 30*time.Second) {
			err = c.syncInventory(refresh, hello.Instance)
			if err == nil {
				requested = false
			}
		}
		cancel()
		if err != nil {
			mark, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_ = c.Store.InventoryFailed(mark)
			stop()
			delay = 5 * time.Second
		}
		if err == nil {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-requests:
			timer.Stop()
			requested = true
		}
	}
}

func (c *Consumer) syncInventory(ctx context.Context, instance string) error {
	accounts, err := c.Inventory.Accounts(ctx)
	if err != nil {
		return err
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if accounts[i].Status == "binding_unavailable" {
					continue
				}
				read, cancel := context.WithTimeout(ctx, 3*time.Second)
				a, e := c.Inventory.Identity(read, accounts[i])
				cancel()
				if e == nil {
					accounts[i] = a
				} else {
					accounts[i].Binding = ""
					accounts[i].Status = "binding_unavailable"
				}
			}
		}()
	}
	for i := range accounts {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err = c.Store.SyncInventory(ctx, accounts); err != nil {
		return err
	}
	publisher, ok := c.Link.(interface {
		PublishBindings(context.Context, string, []store.Account) error
	})
	if !ok {
		return &protocolError{}
	}
	if err = publisher.PublishBindings(ctx, instance, accounts); err != nil {
		return err
	}
	c.inventoryInstance, c.lastInventory = instance, time.Now()
	return nil
}
