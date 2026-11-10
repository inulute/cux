package switcher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

// twoSeatsWithProfiles registers seats "aa" (live in the shared directory)
// and "bb", and gives each an initialised ~/.claude-<alias> profile.
// SwitchTerminal changes this process's environment, so the scope variables
// are registered with t.Setenv first and restored after the test.
func twoSeatsWithProfiles(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv(paths.EnvClaudeConfigDir, "")
	t.Setenv(EnvScope, "")
	setLive(t, "b@example.com", "uuid-b", "token-b")
	if _, _, err := AddCurrent(0, "bb", false, false); err != nil {
		t.Fatalf("add b: %v", err)
	}
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "aa", false, false); err != nil {
		t.Fatalf("add a: %v", err)
	}
	for _, alias := range []string{"aa", "bb"} {
		dir := filepath.Join(paths.Home(), ".claude-"+alias)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		setUpCuxIn(t, dir)
	}
}

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

// loginIn logs configDir in as email with blob, the way Claude Code's own
// /login in that directory would.
func loginIn(t *testing.T, configDir, email, uuid, blob string) {
	t.Helper()
	if err := os.MkdirAll(paths.ClaudeDirIn(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentialsIn(configDir), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(map[string]any{
		"theme":        "dark",
		"oauthAccount": map[string]any{"emailAddress": email, "accountUuid": uuid},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeConfigIn(configDir), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// blobExpiring is a login whose access token expires at ms.
func blobExpiring(token string, ms int64) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"r-%s","expiresAt":%d}}`, token, token, ms)
}

func snapshot(t *testing.T, files ...string) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func assertUnchanged(t *testing.T, files []string, before [][]byte) {
	t.Helper()
	after := snapshot(t, files...)
	for i := range files {
		if !bytes.Equal(before[i], after[i]) {
			t.Errorf("%s changed", files[i])
		}
	}
}

// The shared login, the shared identity file and state.json stay byte for
// byte as they were; only the seat's own profile gets the login.
func TestSwitchTerminalLeavesTheSharedLoginUntouched(t *testing.T) {
	twoSeatsWithProfiles(t)
	shared := []string{paths.ClaudeCredentials(), paths.ClaudeConfig(), paths.StateFile()}
	before := snapshot(t, shared...)

	from, to, err := SwitchTerminal("bb")
	if err != nil {
		t.Fatalf("SwitchTerminal: %v", err)
	}
	if from.Email != "a@example.com" || to.Email != "b@example.com" {
		t.Fatalf("from=%s to=%s", from.Email, to.Email)
	}
	assertUnchanged(t, shared, before)

	profile := filepath.Join(paths.Home(), ".claude-bb")
	if os.Getenv(paths.EnvClaudeConfigDir) != profile || !TerminalScoped() {
		t.Fatalf("env not pointed at %s: CLAUDE_CONFIG_DIR=%q CUX_SCOPE=%q",
			profile, os.Getenv(paths.EnvClaudeConfigDir), os.Getenv(EnvScope))
	}
	if b, err := os.ReadFile(filepath.Join(profile, ".credentials.json")); err != nil || !bytes.Contains(b, []byte("token-b")) {
		t.Fatalf("profile login = %s, %v", b, err)
	}
	cfg, err := os.ReadFile(filepath.Join(profile, ".claude.json"))
	if err != nil || !bytes.Contains(cfg, []byte(`"theme"`)) || !bytes.Contains(cfg, []byte("b@example.com")) {
		t.Fatalf("profile .claude.json = %s, %v", cfg, err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := LiveSlot(state); got != to.Slot {
		t.Fatalf("profile reads as slot %d, want %d", got, to.Slot)
	}
}

// A profile the seat's own claude keeps refreshing must not be overwritten
// with the older stored copy.
func TestSwitchTerminalKeepsAProfileAlreadyLoggedInAsTheSeat(t *testing.T) {
	twoSeatsWithProfiles(t)
	profile := filepath.Join(paths.Home(), ".claude-bb")
	t.Setenv(paths.EnvClaudeConfigDir, profile)
	setLive(t, "b@example.com", "uuid-b", "token-b-rotated")
	t.Setenv(paths.EnvClaudeConfigDir, "")

	if _, _, err := SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(profile, ".credentials.json")); err != nil || !bytes.Contains(b, []byte("token-b-rotated")) {
		t.Fatalf("profile login was overwritten: %s, %v", b, err)
	}
}

// A refusal leaves the environment where it was, so the terminal is not
// left pointing at a half-prepared profile.
func TestSwitchTerminalRestoresEnvWhenTheProfileIsMissing(t *testing.T) {
	twoSeatsWithProfiles(t)
	if err := os.RemoveAll(filepath.Join(paths.Home(), ".claude-bb")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil {
		t.Fatal("expected a refusal for a missing profile")
	}
	if os.Getenv(paths.EnvClaudeConfigDir) != "" || TerminalScoped() {
		t.Fatalf("env moved after a refusal: CLAUDE_CONFIG_DIR=%q CUX_SCOPE=%q",
			os.Getenv(paths.EnvClaudeConfigDir), os.Getenv(EnvScope))
	}
}

// A profile Claude Code never initialised gets nothing written into it.
func TestSwitchTerminalRefusesAProfileThatWasNeverSetUp(t *testing.T) {
	twoSeatsWithProfiles(t)
	profile := filepath.Join(paths.Home(), ".claude-bb")
	if err := os.Remove(filepath.Join(profile, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil {
		t.Fatal("expected a refusal for an uninitialised profile")
	}
	if _, err := os.Stat(filepath.Join(profile, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("a login was written into a refused profile: %v", err)
	}
}

// A seat without an alias has no profile directory to move to.
func TestSwitchTerminalRefusesASeatWithoutAnAlias(t *testing.T) {
	twoSeatsWithProfiles(t)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	slot := state.FindByAlias("bb")
	acct := state.Accounts[slot]
	acct.Alias = ""
	state.Accounts[slot] = acct
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("b@example.com"); err == nil {
		t.Fatal("expected a refusal for a seat with no alias")
	}
}

// Copying the login this terminal runs on into a second directory would
// leave one refresh token with two holders.
func TestSwitchTerminalRefusesToCopyTheCallersOwnLogin(t *testing.T) {
	twoSeatsWithProfiles(t)
	if _, _, err := SwitchTerminal("aa"); err == nil {
		t.Fatal("expected a refusal: the caller is logged in as aa and the profile is not")
	}
	if _, err := os.Stat(filepath.Join(paths.Home(), ".claude-aa", ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("the caller's login was copied: %v", err)
	}
}

// Automatic swaps inside a terminal-scoped process go through SwitchTo;
// they move that process to the next profile and leave the shared side.
func TestSwitchToStaysInsideATerminalScopedProcess(t *testing.T) {
	twoSeatsWithProfiles(t)
	shared := []string{paths.ClaudeCredentials(), paths.ClaudeConfig(), paths.StateFile()}
	before := snapshot(t, shared...)
	t.Setenv(EnvScope, ScopeTerminal)

	if _, _, err := SwitchTo("bb"); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	t.Setenv(paths.EnvClaudeConfigDir, "")
	assertUnchanged(t, shared, before)
}

// Without a terminal scope SwitchTo is the shared swap it always was.
func TestSwitchToWithoutScopeStillSwapsTheSharedLogin(t *testing.T) {
	twoSeatsWithProfiles(t)
	if _, _, err := SwitchTo("bb"); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	if b, err := os.ReadFile(paths.ClaudeCredentials()); err != nil || !bytes.Contains(b, []byte("token-b")) {
		t.Fatalf("shared login = %s, %v", b, err)
	}
	if TerminalScoped() || os.Getenv(paths.EnvClaudeConfigDir) != "" {
		t.Fatal("a shared swap must not scope the process")
	}
}

func TestParseScopeArgs(t *testing.T) {
	cases := []struct {
		in    []string
		rest  []string
		scope string
		bad   bool
	}{
		{in: []string{"work"}, rest: []string{"work"}},
		{in: []string{"--all", "work"}, rest: []string{"work"}, scope: ScopeAll},
		{in: []string{"work", "-a"}, rest: []string{"work"}, scope: ScopeAll},
		{in: []string{"-t", "2"}, rest: []string{"2"}, scope: ScopeTerminal},
		{in: []string{"--scope", "terminal", "work"}, rest: []string{"work"}, scope: ScopeTerminal},
		{in: []string{"work", "--scope=all"}, rest: []string{"work"}, scope: ScopeAll},
		{in: []string{"--all"}, scope: ScopeAll},
		{in: []string{"--all", "--scope", "all", "x"}, rest: []string{"x"}, scope: ScopeAll},
		{in: []string{"--scope"}, bad: true},
		{in: []string{"--scope", "both"}, bad: true},
		{in: []string{"-t", "-a"}, bad: true},
	}
	for _, c := range cases {
		rest, scope, err := ParseScopeArgs(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%v: want an error", c.in)
			}
			continue
		}
		if err != nil || scope != c.scope || len(rest) != len(c.rest) {
			t.Errorf("%v: rest=%v scope=%q err=%v", c.in, rest, scope, err)
			continue
		}
		for i := range rest {
			if rest[i] != c.rest[i] {
				t.Errorf("%v: rest=%v", c.in, rest)
			}
		}
	}
}
