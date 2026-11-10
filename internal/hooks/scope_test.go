package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
)

// Inside a terminal-scoped session /cux:list reports the seat that session
// runs on; the shared login's seat is another terminal's.
func TestRenderPromptUsageShowsTheScopedSessionsSeat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(t.TempDir(), "config.json"))

	state := &store.State{ActiveSlot: 1, Sequence: []int{1, 2}, Accounts: map[int]store.Account{
		1: {Slot: 1, Email: "shared@x.test"},
		2: {Slot: 2, Email: "mine@x.test", Alias: "mine"},
	}}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(home, ".claude-mine")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"mine@x.test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvClaudeConfigDir, profile)
	t.Setenv(switcher.EnvScope, switcher.ScopeTerminal)

	out, err := renderPromptUsage(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ACTIVE [02]") {
		t.Fatalf("scoped session should report slot 02:\n%s", out)
	}
}
