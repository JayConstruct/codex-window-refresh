//go:build linux && cgo

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codex-window-refresh/internal/v2/daemon"
	"codex-window-refresh/internal/v2/executor"
	"codex-window-refresh/internal/v2/hostlink"
	"codex-window-refresh/internal/v2/ingest"
	"codex-window-refresh/internal/v2/ipc"
	"codex-window-refresh/internal/v2/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "window-refreshd:", err)
		os.Exit(1)
	}
}
func run() error {
	var data, runtime string
	var concurrency, terminalJobs int
	var timeout, historyAge time.Duration
	flag.StringVar(&data, "data-dir", "", "absolute private persistent data directory")
	flag.StringVar(&runtime, "runtime-dir", "", "absolute private socket directory, shared with the native bridge")
	flag.IntVar(&concurrency, "query-concurrency", 8, "global quota HTTP concurrency (1..128)")
	flag.DurationVar(&timeout, "query-timeout", 15*time.Second, "per account quota query deadline")
	flag.DurationVar(&historyAge, "history-retention", 7*24*time.Hour, "terminal job history age (1h..8760h)")
	flag.IntVar(&terminalJobs, "terminal-jobs", 10000, "maximum retained terminal jobs (1..50000)")
	flag.Parse()
	st, err := store.Open(data)
	if err != nil {
		return err
	}
	defer st.Close()
	link := &hostlink.Client{Path: filepath.Join(runtime, "bridge.sock")}
	source := &executor.CPA{Host: link}
	defer source.Close()
	service, err := daemon.New(st, source, daemon.Options{QueryConcurrency: concurrency, QueryTimeout: timeout, Retention: store.Retention{HistoryAge: historyAge, TerminalJobs: terminalJobs}})
	if err != nil {
		return err
	}
	listener, err := ipc.Listen(runtime)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); service.Run(ctx) }()
	eventsDone := make(chan struct{})
	consumer := &ingest.Consumer{Store: st, Link: link, Inventory: source}
	go func() { defer close(eventsDone); consumer.Run(ctx) }()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}()
	fmt.Fprintln(os.Stderr, "window-refreshd: read-only prototype ready")
	err = server.Serve(listener)
	stop()
	<-workerDone
	<-eventsDone
	<-stopped
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
