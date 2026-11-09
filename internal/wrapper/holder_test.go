package wrapper

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

// setUpCuxIn does what `CLAUDE_CONFIG_DIR=<dir> cux setup` does for a
// profile: cux's hooks and /switch in that directory.
func setUpCuxIn(t *testing.T, dir string) {
	t.Helper()
	prev, had := os.LookupEnv(paths.EnvClaudeConfigDir)
	_ = os.Setenv(paths.EnvClaudeConfigDir, dir)
	_, err := hookinstall.Install()
	if had {
		_ = os.Setenv(paths.EnvClaudeConfigDir, prev)
	} else {
		_ = os.Unsetenv(paths.EnvClaudeConfigDir)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "commands", "switch.md"), []byte("/switch"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLogin(t *testing.T, configDir, email, token string) {
	t.Helper()
	if err := os.MkdirAll(paths.ClaudeDirIn(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"r-` + token + `"}}`
	if err := os.WriteFile(paths.ClaudeCredentialsIn(configDir), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeConfigIn(configDir),
		[]byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// /switch to a seat whose login is live in another directory is refused
// before the wrapper is asked to stop claude: copying it into the profile
// would leave one refresh token in two places.
func TestSlashSwitchTerminalRefusesASeatLiveElsewhereBeforeSignalling(t *testing.T) {
	pid, profile := slashFixture(t)
	writeLogin(t, filepath.Join(paths.Home(), ".claude-spare"), "b@x.test", "tb")
	var out bytes.Buffer
	err := SlashSwitch("work", switcher.ScopeTerminal, &out)
	if err == nil || !strings.Contains(err.Error(), "two places") {
		t.Fatalf("SlashSwitch = %v, want a refusal", err)
	}
	if _, ok, _ := signals.Read(pid, signals.SwitchRequested); ok {
		t.Fatal("a refused switch must not signal the wrapper")
	}
	if _, err := os.Stat(paths.ClaudeCredentialsIn(profile)); !os.IsNotExist(err) {
		t.Fatalf("a refused /switch wrote the profile's login: %v", err)
	}
}

// Rotation inside a scoped session starts from, and skips, the seat the
// session runs on — not the shared login's seat. With the manual strategy
// the old code skipped the shared seat and offered this session its own.
func TestRotateFallbackInAScopedSessionSkipsItsOwnSeat(t *testing.T) {
	_, profile := slashFixture(t)
	writeLogin(t, profile, "b@x.test", "tb")
	t.Setenv(paths.EnvClaudeConfigDir, profile)
	t.Setenv(switcher.EnvScope, switcher.ScopeTerminal)

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	got, err := rotateFallback(state, usage.Cache{}, &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Fatalf("rotateFallback = %q, want slot 1 (this session runs on slot 2)", got)
	}
	if order := rotationSlots(state); len(order) == 0 || order[0] != 1 {
		t.Fatalf("rotation order = %v, want it to start after slot 2", order)
	}
}
