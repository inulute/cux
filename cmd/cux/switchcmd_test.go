package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/switcher"
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

// TestCmdSwitchHelperProcess is not a test: runCmdSwitch re-runs this test
// binary into it, so `cux switch` can exit, exec and print the way it does
// for a user, and the parent can read the exit code.
func TestCmdSwitchHelperProcess(t *testing.T) {
	raw := os.Getenv("CUX_TEST_CMD_SWITCH_ARGS")
	if raw == "" {
		return
	}
	var args []string
	var env map[string]string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		os.Exit(90)
	}
	// TestMain clears these for every test; this process sets them back.
	if err := json.Unmarshal([]byte(os.Getenv("CUX_TEST_CMD_SWITCH_ENV")), &env); err == nil {
		for k, v := range env {
			_ = os.Setenv(k, v)
		}
	}
	cmdSwitch(args)
	os.Exit(0)
}

func runCmdSwitch(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the subprocess cases use sh")
	}
	a, _ := json.Marshal(args)
	e, _ := json.Marshal(env)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCmdSwitchHelperProcess$")
	cmd.Env = append(os.Environ(), "CUX_TEST_CMD_SWITCH_ARGS="+string(a), "CUX_TEST_CMD_SWITCH_ENV="+string(e))
	var o, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &se
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return o.String(), se.String(), code
}

func sharedLogin(t *testing.T, home string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCmdSwitchScopeFlagsAtTheCommandLevel(t *testing.T) {
	home := seatsForSwitch(t)
	before := sharedLogin(t, home)
	profile := filepath.Join(home, ".claude-bb")

	out, errOut, code := runCmdSwitch(t, nil, "--scope", "terminal", "bb")
	if code != 0 || !strings.Contains(out, "export CLAUDE_CONFIG_DIR='"+profile+"'") {
		t.Fatalf("--scope terminal: code=%d out=%q err=%q", code, out, errOut)
	}
	if !bytes.Equal(before, sharedLogin(t, home)) {
		t.Fatal("--scope terminal rewrote the shared login")
	}

	for _, bad := range [][]string{
		{"-t", "-a", "bb"},
		{"--scope", "both", "bb"},
		{"--scope"},
		{"bb", "cc"},
	} {
		if _, errOut, code := runCmdSwitch(t, nil, bad...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: code=%d err=%q, want usage and exit 2", bad, code, errOut)
		}
	}
	if !bytes.Equal(before, sharedLogin(t, home)) {
		t.Fatal("a rejected command line wrote the shared login")
	}
}

func TestCmdSwitchScopeAllWithEqualsSwapsTheSharedLogin(t *testing.T) {
	home := seatsForSwitch(t)
	if _, errOut, code := runCmdSwitch(t, nil, "--scope=all", "bb"); code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !bytes.Contains(sharedLogin(t, home), []byte("tb")) {
		t.Fatal("--scope=all did not move the shared login")
	}
}

// The command after -- runs on the seat's profile, and its exit code is
// cux's.
func TestCmdSwitchRunsTheCommandOnTheProfile(t *testing.T) {
	home := seatsForSwitch(t)
	out, errOut, code := runCmdSwitch(t, nil, "bb", "--", "sh", "-c", `printf %s "$CLAUDE_CONFIG_DIR $CUX_SCOPE"; exit 3`)
	if code != 3 || out != filepath.Join(home, ".claude-bb")+" terminal" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestCmdSwitchRefusals(t *testing.T) {
	home := seatsForSwitch(t)
	before := sharedLogin(t, home)
	profileA := filepath.Join(home, ".claude-aa")
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{name: "--all with a command", args: []string{"--all", "bb", "--", "true"}, want: "cannot be combined with --all"},
		{name: "inside a cux session", env: map[string]string{"CUX_WRAPPED": "1"}, args: []string{"bb"}, want: "use /switch"},
		{name: "--all from a profile", env: map[string]string{
			paths.EnvClaudeConfigDir: profileA, switcher.EnvScope: switcher.ScopeTerminal,
		}, args: []string{"--all", "bb"}, want: "CUX_SCOPE=terminal"},
		// The shared login is aa: copying it into aa's profile would give
		// its refresh token two holders.
		{name: "seat live in the shared login", args: []string{"aa"}, want: "two places"},
	}
	for _, c := range cases {
		_, errOut, code := runCmdSwitch(t, c.env, c.args...)
		if code != 1 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: code=%d err=%q, want exit 1 and %q", c.name, code, errOut, c.want)
		}
	}
	if !bytes.Equal(before, sharedLogin(t, home)) {
		t.Fatal("a refused switch wrote the shared login")
	}
	if _, err := os.Stat(filepath.Join(profileA, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused switch wrote aa's profile: %v", err)
	}
}

// The eval output carries where the terminal came from, so a later switch
// from inside the profile can still find the shared login.
func TestCmdSwitchEvalOutputRecordsTheSharedDirectory(t *testing.T) {
	seatsForSwitch(t)
	out, errOut, code := runCmdSwitch(t, nil, "bb")
	if code != 0 || !strings.Contains(out, "export CUX_SHARED_CONFIG_DIR=''") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}
