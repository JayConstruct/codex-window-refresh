package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/events"
	"codex-window-refresh/internal/v2/hostlink"
	"codex-window-refresh/internal/v2/store"
)

type fixtureLink struct {
	ackFail      bool
	acknowledged uint64
	ackCalls     int
	afterPull    func()
}

func (*fixtureLink) Handshake(context.Context) (hostlink.Hello, error) {
	return hostlink.Hello{Protocol: 1, Instance: "bridge", HostProcess: "host", ReadOnly: true, Events: true}, nil
}
func (f *fixtureLink) PullEvents(_ context.Context, instance string, after uint64) (events.Batch, error) {
	if f.afterPull != nil {
		f.afterPull()
	}
	b := events.Batch{Instance: instance, HostProcess: "host", After: after, Earliest: 1, Latest: 1, Acknowledged: f.acknowledged}
	if after == 0 {
		b.Events = []events.Event{{Sequence: 1, Kind: "started", RequestID: "r", Identity: "u", At: time.Now()}}
	}
	return b, nil
}
func (f *fixtureLink) AckEvents(_ context.Context, _ string, through uint64) error {
	f.ackCalls++
	if f.ackFail {
		f.ackFail = false
		return errors.New("lost acknowledgement")
	}
	f.acknowledged = through
	return nil
}
func TestLostAckUsesDurableCursorAndDoesNotDuplicateActivity(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	link := &fixtureLink{ackFail: true}
	consumer := Consumer{Store: s, Link: link}
	if err = consumer.Step(context.Background()); err == nil {
		t.Fatal("ack failure hidden")
	}
	c, _ := s.EventCursor(context.Background())
	if c.Sequence != 1 {
		t.Fatal("cursor not durable before ack")
	}
	if err = consumer.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	overview, _ := s.Overview(context.Background())
	if link.acknowledged != 1 || overview.Activity.ActiveRequests != 1 || overview.Activity.RecentUsers != 1 {
		raw, _ := json.Marshal(overview)
		t.Fatal(string(raw))
	}
}
func TestFailedCommitNeverAcknowledgesNativeBuffer(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	link := &fixtureLink{afterPull: func() { _ = s.Close() }}
	if err = (Consumer{Store: s, Link: link}).Step(context.Background()); err == nil {
		t.Fatal("closed storage accepted event")
	}
	if link.ackCalls != 0 {
		t.Fatal("acknowledged uncommitted events")
	}
}
