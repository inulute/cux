package wrapper

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

// slashFixture stands up a wrapped session: two seats, slot 1 live in the
// shared directory, slot 2 aliased "work" with an initialised profile.
// The wrapper pid is this test process, so signals land where Read finds
// them.
func slashFixture(t *testing.T) (wrapperPID int, profile string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))
	t.Setenv(paths.EnvClaudeConfigDir, "")
	t.Setenv(switcher.EnvScope, "")
	t.Setenv(envWrapped, "1")
	t.Setenv(envScopedSwitch, "1")
	wrapperPID = os.Getpid()
	t.Setenv(envWrapperPID, strconv.Itoa(wrapperPID))
	t.Cleanup(func() { signals.CleanupForPID(wrapperPID) })

	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"a@x.test","accountUuid":"u-a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile = filepath.Join(tmp, ".claude-work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".claude.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setUpCuxIn(t, profile)
	state := &store.State{ActiveSlot: 1, Sequence: []int{1, 2}, Accounts: map[int]store.Account{
		1: {Slot: 1, Email: "a@x.test"},
		2: {Slot: 2, Email: "b@x.test", Alias: "work"},
	}}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(signals.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapperPID, profile
}

// A terminal-scoped /switch asks the wrapper to move this session and
// writes nothing shared itself.
func TestSlashSwitchTerminalSignalsTheWrapperWithItsScope(t *testing.T) {
	pid, _ := slashFixture(t)
	stateBefore, err := os.ReadFile(paths.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := SlashSwitch("work", switcher.ScopeTerminal, &out); err != nil {
		t.Fatalf("SlashSwitch: %v", err)
	}
	data, ok, err := signals.Read(pid, signals.SwitchRequested)
	if err != nil || !ok {
		t.Fatalf("switch signal missing: %v (%s)", err, out.String())
	}
	req, err := signals.DecodeSwitchRequested(data)
	if err != nil || req.Target != "work" || req.Scope != switcher.ScopeTerminal {
		t.Fatalf("signal = %+v, %v", req, err)
	}
	if _, err := os.Stat(paths.ClaudeCredentials()); !os.IsNotExist(err) {
		t.Fatalf("a terminal-scoped /switch wrote the shared login: %v", err)
	}
	if stateAfter, _ := os.ReadFile(paths.StateFile()); !bytes.Equal(stateBefore, stateAfter) {
		t.Fatal("a terminal-scoped /switch rewrote state.json")
	}
}

// A wrapper that does not advertise scoped switching would read the signal
// as a shared swap, so the request is refused instead of sent.
func TestSlashSwitchTerminalRefusesAWrapperThatPredatesScopes(t *testing.T) {
	pid, _ := slashFixture(t)
	t.Setenv(envScopedSwitch, "")
	var out bytes.Buffer
	if err := SlashSwitch("work", switcher.ScopeTerminal, &out); err == nil {
		t.Fatal("expected a refusal for a wrapper without scoped switching")
	}
	if _, ok, _ := signals.Read(pid, signals.SwitchRequested); ok {
		t.Fatal("a refused switch must not signal the wrapper")
	}
}

// The profile is checked before the wrapper is asked to stop claude.
func TestSlashSwitchTerminalRefusesAMissingProfileBeforeSignalling(t *testing.T) {
	pid, profile := slashFixture(t)
	if err := os.RemoveAll(profile); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := SlashSwitch("work", switcher.ScopeTerminal, &out); err == nil {
		t.Fatal("expected a refusal for a missing profile")
	}
	if _, ok, _ := signals.Read(pid, signals.SwitchRequested); ok {
		t.Fatal("a refused switch must not signal the wrapper")
	}
}

// --all cannot reach the shared login from a session on a profile.
func TestSlashSwitchAllIsRefusedOnAProfile(t *testing.T) {
	pid, _ := slashFixture(t)
	t.Setenv(switcher.EnvScope, switcher.ScopeTerminal)
	var out bytes.Buffer
	if err := SlashSwitch("work", switcher.ScopeAll, &out); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, ok, _ := signals.Read(pid, signals.SwitchRequested); ok {
		t.Fatal("a refused switch must not signal the wrapper")
	}
	if _, err := os.Stat(paths.ClaudeCredentials()); !os.IsNotExist(err) {
		t.Fatalf("a refused --all wrote the shared login: %v", err)
	}
}

// A terminal-scoped request reaches the profile switcher, never the shared
// one.
func TestCompleteSwapTakesTheTerminalPathForATerminalRequest(t *testing.T) {
	accounts := threeSeats()
	swapFixture(t, accounts, usage.Cache{})
	switchTo = func(string) (store.Account, store.Account, error) {
		t.Fatal("a terminal-scoped swap reached the shared switcher")
		return store.Account{}, store.Account{}, nil
	}
	var asked []string
	realTerminal := switchTerminal
	t.Cleanup(func() { switchTerminal = realTerminal })
	switchTerminal = func(id string) (store.Account, store.Account, error) {
		asked = append(asked, id)
		return accounts[1], accounts[2], nil
	}
	p := &pending{trigger: history.TriggerManual, explicitTarget: "2", scope: switcher.ScopeTerminal}
	retries := 0
	cfg := config.Defaults()
	_, to, swapped := completeSwap(p, &cfg, true, &retries, &bytes.Buffer{})
	if !swapped || to.Slot != 2 || len(asked) != 1 || asked[0] != "2" {
		t.Fatalf("swapped=%v to=%+v asked=%v", swapped, to, asked)
	}
}

func TestCarryTranscriptsCopiesTheSessionAndItsSidecar(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	project := filepath.Join(from, "projects", "-work-repo")
	if err := os.MkdirAll(filepath.Join(project, "sid-1", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sid-1.jsonl"), []byte("turn 1\nturn 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sid-1", "subagents", "a.jsonl"), []byte("sub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "other.jsonl"), []byte("not asked for"), 0o600); err != nil {
		t.Fatal(err)
	}

	carryTranscripts(from, to, &bytes.Buffer{}, "sid-1", "", "sid-1", "missing")

	dst := filepath.Join(to, "projects", "-work-repo")
	if b, err := os.ReadFile(filepath.Join(dst, "sid-1.jsonl")); err != nil || string(b) != "turn 1\nturn 2\n" {
		t.Fatalf("transcript = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sid-1", "subagents", "a.jsonl")); err != nil || string(b) != "sub" {
		t.Fatalf("sidecar = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "other.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("an unrelated transcript was copied: %v", err)
	}
}

// A copy the destination already has, at least as new and as long, is the
// one a resumed session may be writing to; it is not replaced.
func TestCarryTranscriptsKeepsANewerDestinationCopy(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "projects", "-p", "sid.jsonl")
	dst := filepath.Join(to, "projects", "-p", "sid.jsonl")
	for _, f := range []string{src, dst} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(src, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("newer and longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, past, past); err != nil {
		t.Fatal(err)
	}
	carryTranscripts(from, to, &bytes.Buffer{}, "sid")
	if b, _ := os.ReadFile(dst); string(b) != "newer and longer" {
		t.Fatalf("destination was replaced: %q", b)
	}
}
