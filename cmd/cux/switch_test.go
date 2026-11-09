package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/switcher"
)

// seatsForSwitch registers "aa" (live in the shared directory) and "bb",
// each with an initialised ~/.claude-<alias> profile.
func seatsForSwitch(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(home, "config.json"))
	t.Setenv("CUX_WRAPPED", "")
	t.Setenv(paths.EnvClaudeConfigDir, "")
	t.Setenv(switcher.EnvScope, "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	login := func(email, token, alias string) {
		blob := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"r-` + token + `"}}`
		if err := os.WriteFile(paths.ClaudeCredentials(), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{"emailAddress": email, "accountUuid": "u-" + alias}})
		if err := os.WriteFile(paths.ClaudeConfig(), cfg, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := switcher.AddCurrent(0, alias, false, false); err != nil {
			t.Fatalf("add %s: %v", email, err)
		}
		dir := filepath.Join(home, ".claude-"+alias)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		setUpCuxIn(t, dir)
	}
	login("b@example.com", "tb", "bb")
	login("a@example.com", "ta", "aa")
	return home
}

// With no flag and no terminal, `cux switch` prepares the seat's profile
// and prints it for eval; the shared login stays on the previous seat.
func TestCmdSwitchDefaultsToThisTerminalOnly(t *testing.T) {
	home := seatsForSwitch(t)
	shared := filepath.Join(home, ".claude", ".credentials.json")
	before, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdSwitch([]string{"bb"}) })

	profile := filepath.Join(home, ".claude-bb")
	if !strings.Contains(out, "export CLAUDE_CONFIG_DIR='"+profile+"'") ||
		!strings.Contains(out, "export CUX_SCOPE=terminal") {
		t.Fatalf("eval output = %q", out)
	}
	if after, _ := os.ReadFile(shared); !bytes.Equal(before, after) {
		t.Fatal("the default switch rewrote the shared login")
	}
	if b, err := os.ReadFile(filepath.Join(profile, ".credentials.json")); err != nil || !bytes.Contains(b, []byte("tb")) {
		t.Fatalf("profile login = %s, %v", b, err)
	}
}

// --all is the old behaviour: the shared login moves.
func TestCmdSwitchAllSwapsTheSharedLogin(t *testing.T) {
	home := seatsForSwitch(t)
	captureStdout(t, func() { cmdSwitch([]string{"--all", "bb"}) })
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil || !bytes.Contains(b, []byte("tb")) {
		t.Fatalf("shared login = %s, %v", b, err)
	}
	if os.Getenv(switcher.EnvScope) != "" {
		t.Fatal("--all must not scope the process")
	}
}
