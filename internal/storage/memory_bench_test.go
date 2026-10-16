package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func BenchmarkFileStoreMemory(b *testing.B) {
	for _, operation := range []string{"commit", "open-load"} {
		b.Run(operation, func(b *testing.B) {
			ctx := context.Background()
			dir := b.TempDir() + "/state"
			store, err := Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			state := core.CoreState{Requests: map[core.RequestID]core.RequestActivity{}}
			for i := 0; i < 10000; i++ {
				state.Requests[core.RequestID(fmt.Sprint(i))] = core.RequestActivity{Completed: true, LastEventAt: time.Now()}
				state.History = append(state.History, core.HistoryEntry{ID: uint64(i + 1), Prompt: strings.Repeat("x", 1024)})
			}
			state.Revision, err = store.Commit(ctx, 0, state)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if operation == "commit" {
					state.Revision, err = store.Commit(ctx, state.Revision, state)
				} else {
					_ = store.Close()
					store, err = Open(dir)
					if err == nil {
						_, err = store.Load(ctx)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
