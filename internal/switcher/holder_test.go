package switcher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/profile"
	"github.com/inulute/cux/internal/store"
)

// The case the copy guard exists for: this terminal runs on A's profile,
// the shared login is B. Copying B's stored login — the same token the
// shared login holds — into B's profile would give it two holders, and the
// first refresh in either would sign the other out. Refuse, write nothing.
func TestSwitchTerminalRefusesASeatLiveInTheSharedLogin(t *testing.T) {
	twoSeatsWithProfiles(t)
	home := paths.Home()
	profileA, profileB := filepath.Join(home, ".claude-aa"), filepath.Join(home, ".claude-bb")
	loginIn(t, "", "b@example.com", "uuid-b", blobFor("token-b"))
	loginIn(t, profileA, "a@example.com", "uuid-a", blobFor("token-a-own"))
	t.Setenv(paths.EnvClaudeConfigDir, profileA)
	t.Setenv(EnvScope, ScopeTerminal)
	t.Setenv(profile.EnvSharedConfigDir, "")

	watched := []string{
		paths.ClaudeCredentialsIn(""), paths.ClaudeConfigIn(""), paths.StateFile(),
		paths.ClaudeCredentialsIn(profileA), paths.ClaudeConfigIn(profileA),
		paths.ClaudeConfigIn(profileB),
	}
	before := snapshot(t, watched...)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	b := state.Accounts[state.FindByAlias("bb")]
	backupBefore, err := creds.ReadBackup(b.Slot, b.Email)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = SwitchTerminal("bb")
	if err == nil || !strings.Contains(err.Error(), "two places") {
		t.Fatalf("SwitchTerminal(bb) = %v, want a refusal naming the second holder", err)
	}
	assertUnchanged(t, watched, before)
	if _, err := os.Stat(paths.ClaudeCredentialsIn(profileB)); !os.IsNotExist(err) {
		t.Fatalf("B's login was copied into its profile: %v", err)
	}
	if backupAfter, _ := creds.ReadBackup(b.Slot, b.Email); backupAfter != backupBefore {
		t.Fatal("B's backup changed on a refusal")
	}
	if os.Getenv(paths.EnvClaudeConfigDir) != profileA || !TerminalScoped() {
		t.Fatal("a refusal moved this terminal")
	}
}

// Another profile holding the seat counts the same as the shared login.
func TestSwitchTerminalRefusesASeatLiveInAnotherProfile(t *testing.T) {
	twoSeatsWithProfiles(t)
	other := filepath.Join(paths.Home(), ".claude-spare")
	loginIn(t, other, "b@example.com", "uuid-b", blobFor("token-b"))
	if _, _, err := SwitchTerminal("bb"); err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("SwitchTerminal(bb) = %v, want a refusal naming %s", err, other)
	}
	if _, err := os.Stat(paths.ClaudeCredentialsIn(filepath.Join(paths.Home(), ".claude-bb"))); !os.IsNotExist(err) {
		t.Fatalf("B's login was copied: %v", err)
	}
}

// A login that cannot refresh cannot revoke anyone, so it does not block.
func TestSwitchTerminalIgnoresAHolderWithoutARefreshToken(t *testing.T) {
	twoSeatsWithProfiles(t)
	loginIn(t, "", "b@example.com", "uuid-b", `{"claudeAiOauth":{"accessToken":"expired-b"}}`)
	if _, _, err := SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal(bb): %v", err)
	}
}

// A slash /switch is refused before the wrapper stops claude, for the same
// reason, and the check writes nothing.
func TestPrepareTerminalRefusesWithoutWriting(t *testing.T) {
	twoSeatsWithProfiles(t)
	loginIn(t, "", "b@example.com", "uuid-b", blobFor("token-b"))
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareTerminal(state, state.Accounts[state.FindByAlias("bb")]); err == nil {
		t.Fatal("PrepareTerminal accepted a seat live in the shared login")
	}
	if _, err := os.Stat(paths.ClaudeCredentialsIn(filepath.Join(paths.Home(), ".claude-bb"))); !os.IsNotExist(err) {
		t.Fatalf("PrepareTerminal wrote a login: %v", err)
	}
}

// The reverse direction: a seat living in its own profile is not copied
// into the shared login by --all either.
func TestSwitchToAllRefusesASeatLiveInItsProfile(t *testing.T) {
	twoSeatsWithProfiles(t)
	profileB := filepath.Join(paths.Home(), ".claude-bb")
	loginIn(t, profileB, "b@example.com", "uuid-b", blobFor("token-b"))
	shared := []string{paths.ClaudeCredentials(), paths.ClaudeConfig(), paths.StateFile()}
	before := snapshot(t, shared...)
	if _, _, err := SwitchTo("bb"); err == nil || !strings.Contains(err.Error(), profileB) {
		t.Fatalf("SwitchTo(bb) = %v, want a refusal naming %s", err, profileB)
	}
	assertUnchanged(t, shared, before)
}

// A profile cux setup never ran in would start a session with no /switch
// and no automatic switching.
func TestSwitchTerminalRefusesAProfileWithoutCuxSetup(t *testing.T) {
	twoSeatsWithProfiles(t)
	profileB := filepath.Join(paths.Home(), ".claude-bb")
	if err := os.Remove(filepath.Join(profileB, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil || !strings.Contains(err.Error(), "cux setup") {
		t.Fatalf("SwitchTerminal(bb) = %v, want a refusal pointing at cux setup", err)
	}
	if _, err := os.Stat(paths.ClaudeCredentialsIn(profileB)); !os.IsNotExist(err) {
		t.Fatalf("a login was written into a profile without cux: %v", err)
	}

	setUpCuxIn(t, profileB)
	if err := os.Remove(filepath.Join(profileB, "commands", "switch.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil {
		t.Fatal("a profile without /switch was accepted")
	}
}

// When the identity write fails, the copied login comes out again: a
// profile that was not logged in must not keep a token nothing tracks.
func TestSwitchTerminalRemovesTheCopiedLoginWhenTheIdentityWriteFails(t *testing.T) {
	twoSeatsWithProfiles(t)
	profileB := filepath.Join(paths.Home(), ".claude-bb")
	if err := os.WriteFile(paths.ClaudeConfigIn(profileB), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil {
		t.Fatal("expected the identity write to fail")
	}
	if _, err := os.Stat(paths.ClaudeCredentialsIn(profileB)); !os.IsNotExist(err) {
		t.Fatalf("the copied login stayed behind: %v", err)
	}

	// A profile that had credentials of its own (an MCP login) gets them
	// back byte for byte.
	mcpOnly := []byte(`{"mcpOAuth":{"srv":{"refreshToken":"profile-mcp"}}}`)
	if err := os.WriteFile(paths.ClaudeCredentialsIn(profileB), mcpOnly, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err == nil {
		t.Fatal("expected the identity write to fail")
	}
	if b, _ := os.ReadFile(paths.ClaudeCredentialsIn(profileB)); !bytes.Equal(b, mcpOnly) {
		t.Fatalf("profile credentials not restored: %s", b)
	}
	if os.Getenv(paths.EnvClaudeConfigDir) != "" || TerminalScoped() {
		t.Fatal("a failed switch moved this terminal")
	}
}

// Only the account login is copied. MCP logins stored beside it belong to
// the directory they were made in; copying them would put their refresh
// tokens in two places too.
func TestSwitchTerminalCopiesOnlyTheAccountLogin(t *testing.T) {
	twoSeatsWithProfiles(t)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	b := state.Accounts[state.FindByAlias("bb")]
	withMCP := `{"claudeAiOauth":{"accessToken":"token-b","refreshToken":"r-token-b"},"mcpOAuth":{"srv":{"refreshToken":"shared-mcp"}}}`
	if err := creds.WriteBackup(b.Slot, b.Email, withMCP); err != nil {
		t.Fatal(err)
	}
	profileB := filepath.Join(paths.Home(), ".claude-bb")
	if err := os.WriteFile(paths.ClaudeCredentialsIn(profileB), []byte(`{"mcpOAuth":{"srv":{"refreshToken":"profile-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal: %v", err)
	}
	got, err := os.ReadFile(paths.ClaudeCredentialsIn(profileB))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("r-token-b")) || !bytes.Contains(got, []byte("profile-mcp")) || bytes.Contains(got, []byte("shared-mcp")) {
		t.Fatalf("profile credentials = %s", got)
	}
}

// Leaving a seat's own profile saves the login its claude refreshed into
// the backup, as a shared swap saves the seat it leaves. Otherwise the
// backup keeps a revoked token and a later copy signs the profile out.
func TestSwitchTerminalSavesTheProfileLoginItLeaves(t *testing.T) {
	twoSeatsWithProfiles(t)
	home := paths.Home()
	profileA, profileB := filepath.Join(home, ".claude-aa"), filepath.Join(home, ".claude-bb")
	rotated := blobExpiring("token-b-rotated", time.Now().Add(time.Hour).UnixMilli())
	loginIn(t, profileB, "b@example.com", "uuid-b", rotated)
	loginIn(t, profileA, "a@example.com", "uuid-a", blobFor("token-a"))
	t.Setenv(paths.EnvClaudeConfigDir, profileB)
	t.Setenv(EnvScope, ScopeTerminal)

	if _, _, err := SwitchTerminal("aa"); err != nil {
		t.Fatalf("SwitchTerminal(aa): %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	b := state.Accounts[state.FindByAlias("bb")]
	backup, err := creds.ReadBackup(b.Slot, b.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !creds.SameLogin(backup, rotated) {
		t.Fatalf("B's backup was not brought up to its profile's login: %s", backup)
	}
	if os.Getenv(paths.EnvClaudeConfigDir) != profileA {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %s", os.Getenv(paths.EnvClaudeConfigDir), profileA)
	}
}

// The first scoped switch records where the terminal came from, so the
// shared login can still be found from inside a profile.
func TestSwitchTerminalRecordsTheSharedDirectoryOnce(t *testing.T) {
	twoSeatsWithProfiles(t)
	t.Setenv(profile.EnvSharedConfigDir, "")
	if _, _, err := SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal(bb): %v", err)
	}
	if v, ok := os.LookupEnv(profile.EnvSharedConfigDir); !ok || v != "" {
		t.Fatalf("%s = %q, %v; want the default directory recorded", profile.EnvSharedConfigDir, v, ok)
	}
	if _, _, err := SwitchTerminal("aa"); err == nil {
		t.Fatal("aa is live in the shared login; the copy must be refused from a profile too")
	}
}
