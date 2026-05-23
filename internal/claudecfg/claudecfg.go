// Package claudecfg manipulates the oauthAccount block inside Claude
// Code's global config (~/.claude/.claude.json), without touching any of
// the surrounding settings (themes, MCP servers, history, etc.).
//
// We treat the file as an opaque map[string]interface{} and only swap
// the "oauthAccount" key. This is the same surgical approach
// cc-account-switcher uses, and it's the only safe one — the file has
// dozens of unrelated keys we have no business rewriting.
package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// OAuthAccount is the subset of the oauthAccount block we identify
// accounts by. The block has more fields in practice; we copy them all
// through unchanged via json.RawMessage rather than modelling them.
type OAuthAccount struct {
	EmailAddress     string `json:"emailAddress"`
	AccountUUID      string `json:"accountUuid"`
	OrganizationUUID string `json:"organizationUuid"`
	DisplayName      string `json:"displayName"`
	OrganizationName string `json:"organizationName"`
}

// ErrNoConfig is returned when Claude Code's config file does not exist
// (user has never launched claude or has fully purged it).
var ErrNoConfig = errors.New("claudecfg: ~/.claude/.claude.json not found")

// ErrNoOAuthAccount is returned when the config exists but has no
// oauthAccount block (user is logged out).
var ErrNoOAuthAccount = errors.New("claudecfg: oauthAccount block not present")

// ReadOAuthBlock returns the oauthAccount block as raw JSON, plus the
// parsed identifying fields. The raw JSON is what we store in backups
// so unknown fields round-trip without loss.
func ReadOAuthBlock() (raw json.RawMessage, parsed OAuthAccount, err error) {
	cfg, err := readConfig()
	if err != nil {
		return nil, OAuthAccount{}, err
	}
	rawBlock, ok := cfg["oauthAccount"]
	if !ok {
		return nil, OAuthAccount{}, ErrNoOAuthAccount
	}
	if err := json.Unmarshal(rawBlock, &parsed); err != nil {
		return nil, OAuthAccount{}, fmt.Errorf("claudecfg: parse oauthAccount: %w", err)
	}
	return rawBlock, parsed, nil
}

// WriteOAuthBlock replaces the oauthAccount block in-place. The rest of
// the config file is untouched. If the file does not exist, returns
// ErrNoConfig — we never create a Claude Code config from scratch.
func WriteOAuthBlock(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("claudecfg: refusing to write empty oauthAccount block")
	}
	cfg, err := readConfig()
	if err != nil {
		return err
	}
	// json.RawMessage round-trips through map[string]json.RawMessage
	// without re-marshalling unrelated values.
	cfg["oauthAccount"] = raw

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("claudecfg: marshal: %w", err)
	}
	return atomicfile.Write(paths.ClaudeConfig(), out, 0o600)
}

// readConfig parses the config as a map preserving raw JSON for every
// value, so we can rewrite oauthAccount without disturbing anything else.
func readConfig() (map[string]json.RawMessage, error) {
	path := paths.ClaudeConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoConfig
		}
		return nil, fmt.Errorf("claudecfg: read %s: %w", path, err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("claudecfg: parse %s: %w", path, err)
	}
	return cfg, nil
}
