// Package config persists user preferences for cux at
// $XDG_CONFIG_HOME/cux/config.json (or $HOME/.config/cux/config.json
// when XDG_CONFIG_HOME is unset). It owns no business logic — it just
// reads, writes, and merges with defaults so the rest of the codebase
// can ask "what does the user want me to do" without per-call defaults.
//
// Defaults match claude-revolver's so users porting from there see
// familiar behavior:
//
//   - thresholds: 100% (5h), 100% (7d)
//   - strategy: drain, no fixed order (auto by highest 7d)
//   - auto_resume: true
//   - auto_message: "Go continue."   (set to "" to skip the prompt
//     and just resume silently)
//   - notify: true                    (no-op in v0.2; reserved for v0.3
//     when desktop notifications land)
//   - poll_interval_seconds: 60       (no-op until the monitor lands;
//     kept here so users can tune it
//     ahead of v0.3)
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/strategy"
	"github.com/inulute/cux/internal/usage"
)

// StrategyConfig is the on-disk form of a strategy choice. We keep it
// stringly-typed (Kind = "drain"|"balanced"|"manual") so the JSON file
// is human-editable without needing to know the Go enum values.
type StrategyConfig struct {
	Kind  string   `json:"kind"`
	Order []string `json:"order"` // emails in priority order, drain mode only
}

// UpdateCheckConfig controls whether cux checks for newer releases on
// the GitHub repo. Off by default so installing cux never causes a
// network call the user didn't ask for. `cux setup` offers to enable
// it interactively. The cadence cap means we ping GitHub at most once
// per CadenceHours regardless of how often `cux` runs.
type UpdateCheckConfig struct {
	Enabled      bool `json:"enabled"`
	CadenceHours int  `json:"cadence_hours"`
}

// Config is the full preferences shape.
//
// Auto-switch toggles vs strategy.kind: setting `strategy.kind=manual`
// also disables manual /switch *rotation* (the user must always supply
// an explicit target). The two `auto_switch_on_*` booleans below are
// the right knobs when you want auto-swap off but `/switch` rotation
// to keep working.
type Config struct {
	Thresholds            usage.Thresholds  `json:"thresholds"`
	Strategy              StrategyConfig    `json:"strategy"`
	AutoSwitchOnThreshold bool              `json:"auto_switch_on_threshold"`
	AutoSwitchOnRateLimit bool              `json:"auto_switch_on_rate_limit"`
	AutoResume            bool              `json:"auto_resume"`
	AutoMessage           string            `json:"auto_message"`
	RetryOnAPIError       bool              `json:"retry_on_api_error"`
	Notify                bool              `json:"notify"`
	PollIntervalSeconds   int               `json:"poll_interval_seconds"`
	UpdateCheck           UpdateCheckConfig `json:"update_check"`
	Theme                 string            `json:"theme"`
}

// ResolvedStrategy returns the parsed strategy.Kind.
func (c Config) ResolvedStrategy() strategy.Kind {
	return strategy.ParseKind(c.Strategy.Kind)
}

// Defaults returns a fresh Config with all fields at their default
// values. Used by Load when the file is missing and as the merge base
// when individual fields are missing in a partially-written file.
func Defaults() Config {
	return Config{
		Thresholds:            usage.DefaultThresholds(),
		Strategy:              StrategyConfig{Kind: "drain", Order: []string{}},
		AutoSwitchOnThreshold: true,
		AutoSwitchOnRateLimit: true,
		AutoResume:            true,
		AutoMessage:           "Go continue.",
		RetryOnAPIError:       true,
		Notify:                true,
		PollIntervalSeconds:   60,
		UpdateCheck:           UpdateCheckConfig{Enabled: true, CadenceHours: 6},
		Theme:                 "claude",
	}
}

// Load returns the user's config merged with defaults. A missing file
// yields the defaults — first run requires no setup.
func Load() (Config, error) {
	def := Defaults()
	b, err := os.ReadFile(filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, fmt.Errorf("config: read: %w", err)
	}
	if len(b) == 0 {
		return def, nil
	}
	cfg := def
	// Decode on top of the defaults so any field omitted from the file
	// stays at its default value.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return def, fmt.Errorf("config: parse: %w", err)
	}
	if cfg.Strategy.Kind == "" {
		cfg.Strategy.Kind = def.Strategy.Kind
	}
	if cfg.Strategy.Order == nil {
		cfg.Strategy.Order = []string{}
	}
	return cfg, nil
}

// Save writes the current config atomically at mode 0600.
func Save(c Config) error {
	dir := filepath.Dir(filePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	return atomicfile.Write(filePath(), data, 0o600)
}

// Set applies a single dotted-path update to an existing config and
// returns the modified config. The complete set of recognised keys is
// listed in `Keys()` — Set and Keys are kept in lockstep.
func Set(c Config, key, value string) (Config, error) {
	switch key {
	case "thresholds.five_hour":
		n, err := parsePct(value)
		if err != nil {
			return c, err
		}
		c.Thresholds.FiveHour = n
	case "thresholds.seven_day":
		n, err := parsePct(value)
		if err != nil {
			return c, err
		}
		c.Thresholds.SevenDay = n
	case "strategy.kind":
		v := strings.ToLower(strings.TrimSpace(value))
		switch v {
		case "drain", "balanced", "manual":
			c.Strategy.Kind = v
		default:
			return c, fmt.Errorf("config: strategy.kind must be drain|balanced|manual, got %q", value)
		}
	case "strategy.order":
		v := strings.TrimSpace(value)
		if v == "" || v == `""` {
			c.Strategy.Order = []string{}
		} else {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			c.Strategy.Order = out
		}
	case "auto_switch_on_threshold":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.AutoSwitchOnThreshold = b
	case "auto_switch_on_rate_limit":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.AutoSwitchOnRateLimit = b
	case "auto_resume":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.AutoResume = b
	case "auto_message":
		// Accept a literal empty string as "no auto-prompt." We treat
		// the bare token "" specially because shells often strip
		// quotes; users who want a literal `""` value would not write
		// that anyway.
		if value == `""` {
			c.AutoMessage = ""
		} else {
			c.AutoMessage = value
		}
	case "retry_on_api_error":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.RetryOnAPIError = b
	case "notify":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.Notify = b
	case "update_check.enabled":
		b, err := parseBool(value)
		if err != nil {
			return c, err
		}
		c.UpdateCheck.Enabled = b
	case "update_check.cadence_hours":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return c, fmt.Errorf("config: update_check.cadence_hours must be a positive integer, got %q", value)
		}
		c.UpdateCheck.CadenceHours = n
	case "poll_interval_seconds":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return c, fmt.Errorf("config: poll_interval_seconds must be a non-negative integer, got %q", value)
		}
		c.PollIntervalSeconds = n
	case "theme":
		v := strings.ToLower(strings.TrimSpace(value))
		switch v {
		case "default", "claude":
			c.Theme = v
		default:
			return c, fmt.Errorf("config: theme must be default|claude, got %q", value)
		}
	default:
		return c, fmt.Errorf("config: unknown key %q", key)
	}
	return c, nil
}

// KeyInfo is one row in the configurable-keys catalogue printed by
// `cux config keys`. Default is the value as a string for display
// purposes only — actual defaults live in Defaults().
type KeyInfo struct {
	Key         string
	Default     string
	Description string
	Current     string // populated by Keys() based on the live config
}

// Keys returns metadata for every settable key, with the current
// value already filled in from c. Listed in the same order users will
// see in `cux config keys` — keep stable so docs and screenshots age
// well.
func Keys(c Config) []KeyInfo {
	return []KeyInfo{
		{
			Key: "thresholds.five_hour", Default: "100",
			Description: "auto-swap when 5h utilisation reaches this % (100 = reactive only)",
			Current:     strconv.Itoa(c.Thresholds.FiveHour),
		},
		{
			Key: "thresholds.seven_day", Default: "100",
			Description: "auto-swap when 7d utilisation reaches this % (100 = reactive only)",
			Current:     strconv.Itoa(c.Thresholds.SevenDay),
		},
		{
			Key: "strategy.kind", Default: "drain",
			Description: "drain | balanced | manual",
			Current:     c.Strategy.Kind,
		},
		{
			Key: "strategy.order", Default: "(empty)",
			Description: "drain mode priority: comma-separated emails; empty = auto by 7d",
			Current:     strings.Join(c.Strategy.Order, ","),
		},
		{
			Key: "auto_switch_on_threshold", Default: "true",
			Description: "swap pre-emptively when usage crosses thresholds",
			Current:     strconv.FormatBool(c.AutoSwitchOnThreshold),
		},
		{
			Key: "auto_switch_on_rate_limit", Default: "true",
			Description: "swap when the API returns a rate-limit error",
			Current:     strconv.FormatBool(c.AutoSwitchOnRateLimit),
		},
		{
			Key: "auto_resume", Default: "true",
			Description: "pass --resume <id> to the relaunched claude after a swap",
			Current:     strconv.FormatBool(c.AutoResume),
		},
		{
			Key: "auto_message", Default: "Go continue.",
			Description: `first user turn after auto-swap; set to "" for silent resume`,
			Current:     c.AutoMessage,
		},
		{
			Key: "retry_on_api_error", Default: "true",
			Description: "relaunch and auto-continue after a non-rate-limit API failure (fibonacci backoff)",
			Current:     strconv.FormatBool(c.RetryOnAPIError),
		},
		{
			Key: "notify", Default: "true",
			Description: "desktop notifications on swap (reserved for v0.3)",
			Current:     strconv.FormatBool(c.Notify),
		},
		{
			Key: "poll_interval_seconds", Default: "60",
			Description: "background usage poll interval (reserved for v0.3)",
			Current:     strconv.Itoa(c.PollIntervalSeconds),
		},
		{
			Key: "update_check.enabled", Default: "false",
			Description: "check GitHub for newer cux releases on startup",
			Current:     strconv.FormatBool(c.UpdateCheck.Enabled),
		},
		{
			Key: "update_check.cadence_hours", Default: "6",
			Description: "minimum hours between update checks (cached locally)",
			Current:     strconv.Itoa(c.UpdateCheck.CadenceHours),
		},
		{
			Key: "theme", Default: "default",
			Description: "visual style: default | claude",
			Current:     c.Theme,
		},
	}
}

// --- internals -----------------------------------------------------------

func filePath() string {
	if v := os.Getenv("CUX_CONFIG_FILE"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fall back to /tmp; subsequent reads will fail explicitly.
			return filepath.Join(os.TempDir(), "cux-config.json")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "cux", "config.json")
}

func parsePct(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(s, "%")))
	if err != nil {
		return 0, fmt.Errorf("config: expected integer percentage 0–100, got %q", s)
	}
	if n < 0 || n > 100 {
		return 0, errors.New("config: percentage out of range (must be 0–100)")
	}
	return n, nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("config: expected boolean, got %q", s)
}
