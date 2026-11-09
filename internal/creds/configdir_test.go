package creds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inulute/cux/internal/paths"
)

// Claude Code keys its macOS login item by CLAUDE_CONFIG_DIR. Reading the
// unsuffixed item from inside such a directory would read the default
// login instead, and a switch would overwrite it for every other session.
func TestLiveKeychainServicesFollowClaudeConfigDir(t *testing.T) {
	t.Setenv(paths.EnvClaudeConfigDir, "")
	got := liveKeychainServices()
	if len(got) != len(macKeychainServices) || got[0] != "Claude Code-credentials" {
		t.Fatalf("default services = %v, want %v", got, macKeychainServices)
	}

	// Expected suffix: first 8 hex digits of sha256("/home/u/.claude-work"),
	// the same derivation Claude Code uses.
	t.Setenv(paths.EnvClaudeConfigDir, "/home/u/.claude-work")
	got = liveKeychainServices()
	if len(got) != 1 || got[0] != "Claude Code-credentials-9fa37e90" {
		t.Fatalf("services under CLAUDE_CONFIG_DIR = %v, want only the directory's own item", got)
	}
}

// The file backend reads and writes the login inside CLAUDE_CONFIG_DIR, so a
// write there leaves the default ~/.claude login alone.
func TestFileBackendWritesInsideClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv(paths.EnvClaudeConfigDir, "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteLive(accountBlob); err != nil {
		t.Fatalf("default WriteLive: %v", err)
	}

	profile := filepath.Join(home, ".claude-work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvClaudeConfigDir, profile)
	other := `{"claudeAiOauth":{"accessToken":"tok-work","refreshToken":"r-work"}}`
	if err := WriteLive(other); err != nil {
		t.Fatalf("profile WriteLive: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(profile, ".credentials.json")); err != nil || string(b) != other {
		t.Fatalf("profile login = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")); err != nil || string(b) != accountBlob {
		t.Fatalf("default login changed: %q, %v", b, err)
	}
}
