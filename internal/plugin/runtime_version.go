package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const HotReloadProtocol = 2

type runtimeVersion struct {
	Protocol     int    `json:"hot_reload_protocol"`
	Version      string `json:"version"`
	Phase        string `json:"phase"`
	InstanceID   string `json:"instance_id"`
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start"`
	BootID       string `json:"boot_id"`
	LibraryPath  string `json:"library_path"`
	ConfigFile   string `json:"config_file"`
	StateDir     string `json:"state_dir"`
	ReloadToken  string `json:"reload_token"`
	UpdatedAt    string `json:"updated_at"`
}

func newRuntimeInstance() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func (w WrapperConfig) reloadToken() string {
	n := w.HostStore
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "local-reload-token" && n.Content[i+1].Kind == yaml.ScalarNode {
				value := n.Content[i+1].Value
				if len(value) == 64 {
					if _, err := hex.DecodeString(value); err == nil {
						return value
					}
				}
			}
		}
	}
	return ""
}

// worker is nonnil only for an asynchronous worker exit. Serialize publication
// independently of lifecycle: quiesce waits for this worker while owning the
// lifecycle mutex, and must publish its final receipt before releasing the lock.
func (s *Service) publishRuntimeVersion(phase string, worker chan struct{}) error {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	s.mu.RLock()
	wrapper, store, instance := s.wrapper, s.store, s.instanceID
	skip := worker != nil && (worker != s.done || s.stopping)
	s.mu.RUnlock()
	if skip || store == nil {
		return nil
	}
	publisher, ok := store.(interface{ PublishRuntimeVersion([]byte) error })
	if !ok { // Mock stores and non-native embeddings may omit local reporting.
		return nil
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return errors.New("runtime process identity unavailable")
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return errors.New("runtime process identity invalid")
	}
	fields := strings.Fields(string(stat)[end+1:])
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(fields) < 20 {
		return errors.New("runtime process identity unavailable")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return errors.New("runtime process identity invalid")
	}
	library := s.options.LibraryPath
	if library != "" {
		library, err = filepath.Abs(library)
		if err != nil {
			return errors.New("runtime library identity invalid")
		}
	}
	raw, err := json.Marshal(runtimeVersion{Protocol: HotReloadProtocol, Version: Version, Phase: phase,
		InstanceID: instance, PID: os.Getpid(), ProcessStart: fields[19], BootID: strings.TrimSpace(string(boot)),
		LibraryPath: library, ConfigFile: wrapper.ConfigFile, StateDir: wrapper.StateDir,
		ReloadToken: wrapper.reloadToken(), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	if err := publisher.PublishRuntimeVersion(raw); err != nil {
		return errors.New("runtime version file publication failed")
	}
	return nil
}
