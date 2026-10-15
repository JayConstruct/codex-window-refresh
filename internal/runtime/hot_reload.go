package runtime

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

// Linux process identity remains stable across independently mapped libraries,
// while a fresh CPA process (including a reused PID) cannot inherit busy requests.
func hotReloadProcessIdentity() string {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(stat)[end+1:])
	if len(fields) < 20 {
		return ""
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(boot)) + ":" + strconv.Itoa(os.Getpid()) + ":" + fields[19]
}

// PrepareHotReload runs after the worker and admitted hooks have drained. It
// performs a local durable handoff only, never quota queries or model execution.
func (e *Engine) PrepareHotReload(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.poisoned {
		return errors.New("durable_commit_failed")
	}
	identity := hotReloadProcessIdentity()
	if identity == "" {
		return errors.New("hot_reload_process_identity_unavailable")
	}
	e.drain()
	e.state.HotReloadProcess = identity
	return e.commit(ctx)
}
