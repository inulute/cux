package hooks

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

func TestRenderPromptSupportIncludesURL(t *testing.T) {
	out := renderPromptSupport()
	if !strings.Contains(out, "https://support.inulute.com") {
		t.Fatalf("support output missing URL: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("prompt support output contained ANSI escape bytes: %q", out)
	}
}

func TestRenderPromptUsageReportsAllExhaustedAtEffectiveCaps(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 2,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": hookAccountUsage(94, 67),
		"b@x.test": hookAccountUsage(0, 100),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := renderPromptUsage(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "STATUS : ALL MANAGED ACCOUNTS EXHAUSTED") {
		t.Fatalf("status did not report exhaustion:\n%s", out)
	}
	if strings.Contains(out, "NEXT USABLE") {
		t.Fatalf("status should not advertise a next usable account:\n%s", out)
	}
	if !strings.Contains(out, "a@x.test") || !strings.Contains(out, "FULL") {
		t.Fatalf("status should mark the threshold-exhausted account full:\n%s", out)
	}
}

func TestUserPromptSubmitBareSwitchBlocksWhenAllAccountsExhausted(t *testing.T) {
	t.Setenv("CUX_WRAPPED", "1")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 2,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": hookAccountUsage(94, 67),
		"b@x.test": hookAccountUsage(0, 100),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := UserPromptSubmit(strings.NewReader(`{"prompt":"/switch"}`), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"decision":"block"`) {
		t.Fatalf("hook should block /switch, got: %s", got)
	}
	if strings.Contains(got, "switching accounts") {
		t.Fatalf("hook should not request a switch, got: %s", got)
	}
	if !strings.Contains(got, "STATUS : ALL MANAGED ACCOUNTS EXHAUSTED") {
		t.Fatalf("hook should return exhausted status, got: %s", got)
	}
	if strings.Contains(got, "CUX_WRAPPER_PID") {
		t.Fatalf("hook should not reach switch signaling path, got: %s", got)
	}
}

// TestHandleAutoSwitchPrompt_HardBlock_Threshold100 verifies that when the
// active account is at 100% 5h utilization AND thresholds are set to 100
// (default/"reactive-only"), the prompt-submit hook switches credentials
// in-place and approves the prompt (empty stdout) so Claude Code sends it
// on the new account without any manual resend.
//
// This is the "session limit" regression: Claude Code's session-limit UI
// blocks before any tool use, so PostToolUseFailure never fires. The
// prompt-submit hook must catch the hard-blocked case itself.
func TestHandleAutoSwitchPrompt_HardBlock_Threshold100(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CUX_WRAPPED", "1")
	// Redirect HOME and XDG_DATA_HOME so all paths resolve under tmp.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))

	// Write a minimal fake Claude config so CurrentLiveEmail() returns
	// the blocked account's email.
	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeJSON := `{"oauthAccount":{"emailAddress":"blocked@x.test","accountUuid":"u1"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// Backup credentials for slot 2 (target account). The hook calls
	// switcher.SwitchTo directly, which reads these files to swap creds.
	// The credential blob intentionally omits "accessToken" so that
	// RefreshAll's refreshOne call hits ExtractAccessToken error and
	// returns before making a real API call — avoiding a 401 that would
	// mark the account as TokenExpired and prevent PickNext from selecting it.
	acct2Dir := filepath.Join(tmp, "cux", "accounts", "02-free@x.test")
	if err := os.MkdirAll(acct2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCreds := `{"cux-test-slot":"2"}` // no accessToken → no API call
	if err := os.WriteFile(filepath.Join(acct2Dir, "credentials.json"), []byte(fakeCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeOAuth := `{"emailAddress":"free@x.test","accountUuid":"u2"}`
	if err := os.WriteFile(filepath.Join(acct2Dir, "oauth.json"), []byte(fakeOAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	// Two accounts: slot 1 active and hard-blocked at 100% 5h, slot 2 free.
	state := &store.State{
		ActiveSlot: 1,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "blocked@x.test"},
			2: {Slot: 2, Email: "free@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(usage.Cache{
		"blocked@x.test": hookAccountUsage(100, 31), // 5h at hard limit
		"free@x.test":    hookAccountUsage(0, 50),   // plenty of room
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := UserPromptSubmit(strings.NewReader(`{"prompt":"do something"}`), &out); err != nil {
		t.Fatalf("UserPromptSubmit returned error: %v", err)
	}
	got := out.String()

	// Hook must NOT block the prompt — empty stdout means Claude Code approves it
	// and sends it using the newly-swapped credentials.
	if strings.Contains(got, `"decision":"block"`) {
		t.Fatalf("hook should approve (empty stdout), but got block: %s", got)
	}
	if got != "" {
		t.Fatalf("hook should produce empty stdout for an approved switch, got: %s", got)
	}

	// The live credentials file must now contain the target account's backup blob.
	liveCredsPath := filepath.Join(claudeDir, ".credentials.json")
	b, err := os.ReadFile(liveCredsPath)
	if err != nil {
		t.Fatalf("live credentials file not written after switch: %v", err)
	}
	if !strings.Contains(string(b), "cux-test-slot") {
		t.Fatalf("live credentials should contain target account blob, got: %s", b)
	}

	// Live Claude config must now identify the target account.
	claudeCfg, err := os.ReadFile(filepath.Join(claudeDir, ".claude.json"))
	if err != nil {
		t.Fatalf("claude config not readable: %v", err)
	}
	if !strings.Contains(string(claudeCfg), "free@x.test") {
		t.Fatalf("claude config should have been updated to free@x.test, got: %s", claudeCfg)
	}
}

func TestRateLimitStopFailureWritesSignal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CUX_WRAPPED", "1")
	t.Setenv("XDG_DATA_HOME", tmp)
	pid := os.Getpid()
	t.Setenv("CUX_WRAPPER_PID", fmt.Sprintf("%d", pid))

	input := `{
		"hook_event_name": "StopFailure",
		"error": "rate_limit",
		"error_details": "429 Too Many Requests",
		"last_assistant_message": "You've hit your session limit · resets 1:10am (Asia/Kolkata)"
	}`
	if err := RateLimit(strings.NewReader(input)); err != nil {
		t.Fatalf("RateLimit returned error: %v", err)
	}

	b, ok, err := signals.Read(pid, signals.RateLimited)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("RateLimit should write rate-limited signal for StopFailure rate_limit")
	}
	p, err := signals.DecodeRateLimited(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Message, "session limit") {
		t.Fatalf("signal should preserve rendered session-limit message, got %q", p.Message)
	}
}

func hookAccountUsage(five, seven float64) usage.AccountUsage {
	w5 := usage.Window{Utilization: five}
	w7 := usage.Window{Utilization: seven}
	return usage.AccountUsage{FiveHour: &w5, SevenDay: &w7}
}
