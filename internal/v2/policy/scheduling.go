package policy

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrScheduling = errors.New("invalid_scheduling_settings")

type ProjectPreference struct {
	Project     string   `json:"project"`
	Accounts    []string `json:"accounts"`
	Directories []string `json:"directories,omitempty"`
}
type SchedulingConfig struct {
	Enabled            bool                `json:"enabled"`
	PreferReset        bool                `json:"prefer_reset"`
	QuotaMaxAgeSeconds int                 `json:"quota_max_age_seconds"`
	SessionTTLSeconds  int                 `json:"session_ttl_seconds"`
	SubagentAffinity   bool                `json:"subagent_affinity"`
	RebindOn           string              `json:"rebind_on"`
	MaxInflight        int                 `json:"max_inflight"`
	Projects           []ProjectPreference `json:"projects"`
	Cards              ResetConfig         `json:"cards"`
}
type ResetConfig struct {
	Enabled            bool `json:"enabled"`
	AutoConsume        bool `json:"auto_consume"`
	PollSeconds        int  `json:"poll_seconds"`
	ExpirySeconds      int  `json:"expiry_seconds"`
	ForceBeforeSeconds int  `json:"force_before_seconds"`
	MinUsedPercent     int  `json:"min_used_percent"`
}

func DefaultScheduling() SchedulingConfig {
	return SchedulingConfig{PreferReset: true, QuotaMaxAgeSeconds: 300, SessionTTLSeconds: 1800, SubagentAffinity: true, RebindOn: "quota", MaxInflight: 2, Projects: []ProjectPreference{}, Cards: ResetConfig{PollSeconds: 900, ExpirySeconds: 86400, ForceBeforeSeconds: 1800, MinUsedPercent: 80}}
}
func RoutingLabel(s string) bool {
	return len(s) > 0 && len(s) <= 256 && strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == '"' || r == '\\' }) < 0
}

var launcherProject = regexp.MustCompile("^[A-Za-z0-9._:-]{1,128}$")

// ProjectDirectoryKey compares client paths without reading the server filesystem.
// The launcher resolves symlinks on the client when matching the working directory.
func ProjectDirectoryKey(directory string) (string, bool) {
	if directory == "" || directory != strings.TrimSpace(directory) || len(directory) > 4096 || !utf8.ValidString(directory) || strings.IndexFunc(directory, unicode.IsControl) >= 0 {
		return "", false
	}
	if strings.HasPrefix(directory, "/") {
		return path.Clean(directory), true
	}
	windows := strings.ReplaceAll(directory, "\\", "/")
	if len(windows) >= 3 && ((windows[0] >= 'a' && windows[0] <= 'z') || (windows[0] >= 'A' && windows[0] <= 'Z')) && windows[1:3] == ":/" {
		return strings.ToLower(windows[:2] + path.Clean(windows[2:])), true
	}
	if strings.HasPrefix(directory, "\\\\") {
		parts := strings.Split(strings.Trim(windows, "/"), "/")
		if len(parts) >= 2 && parts[0] != "" && parts[0] != "." && parts[0] != ".." && parts[1] != "" && parts[1] != "." && parts[1] != ".." {
			tail := strings.TrimSuffix(path.Clean("/"+strings.Join(parts[2:], "/")), "/")
			return "unc:/" + strings.ToLower(parts[0]+"/"+parts[1]+tail), true
		}
	}
	return "", false
}
func (c SchedulingConfig) Validate() error {
	if c.RebindOn != "quota" && c.RebindOn != "any" {
		return ErrScheduling
	}
	if c.QuotaMaxAgeSeconds < 5 || c.QuotaMaxAgeSeconds > 3600 || c.SessionTTLSeconds < 1 || c.SessionTTLSeconds > 86400 || c.MaxInflight < 1 || c.MaxInflight > 128 || len(c.Projects) > 128 || c.Cards.PollSeconds < 60 || c.Cards.PollSeconds > 86400 || c.Cards.ExpirySeconds < 60 || c.Cards.ExpirySeconds > 30*86400 || c.Cards.ForceBeforeSeconds < 1 || c.Cards.ForceBeforeSeconds > c.Cards.ExpirySeconds || c.Cards.MinUsedPercent < 1 || c.Cards.MinUsedPercent > 100 {
		return ErrScheduling
	}
	seen := map[string]bool{}
	directories := map[string]bool{}
	for _, p := range c.Projects {
		if !RoutingLabel(p.Project) || seen[p.Project] || len(p.Accounts) == 0 || len(p.Accounts) > 1000 || len(p.Directories) > 16 || len(p.Directories) > 0 && !launcherProject.MatchString(p.Project) {
			return ErrScheduling
		}
		seen[p.Project] = true
		for _, directory := range p.Directories {
			key, valid := ProjectDirectoryKey(directory)
			if !valid || directories[key] {
				return ErrScheduling
			}
			directories[key] = true
		}
		ids := map[string]bool{}
		for _, id := range p.Accounts {
			if !RoutingLabel(id) || ids[id] {
				return ErrScheduling
			}
			ids[id] = true
		}
	}
	return nil
}

// ResetCredit deliberately excludes service-provided free text.
type ResetCredit struct {
	ID        string `json:"id"`
	ResetType string `json:"resetType"`
	Status    string `json:"status"`
	ExpiresAt *int64 `json:"expiresAt"`
}
type ResetCredits struct {
	AvailableCount int           `json:"availableCount"`
	Credits        []ResetCredit `json:"credits"`
}
