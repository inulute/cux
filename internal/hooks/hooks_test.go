package hooks

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/paths"
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
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
	// Redirect HOME and XDG_DATA_HOME so all paths resolve under tmp,
	// and force the file credential backend so macOS does not read or
	// write the real keychain (issue #7).
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
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

	// RefreshAll runs inside the hook, so point the usage API at a local
	// server that always fails. The seeded cache below then survives the
	// refresh — refreshOne returns the fetch error without caching — which
	// is what this test needs, and it stays hermetic.
	//
	// This used to be achieved with a credential blob that had no
	// accessToken, so refreshOne bailed before the API call. That blob is no
	// longer writable: creds.WriteLive refuses credentials with no account
	// token, because writing them over a live login silently signs the user
	// out (issue #42).
	apiDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "usage endpoint unavailable in test", http.StatusInternalServerError)
	}))
	defer apiDown.Close()
	t.Setenv("CUX_USAGE_ENDPOINT", apiDown.URL)

	// Backup credentials for slot 2 (target account). The hook calls
	// switcher.SwitchTo directly, which reads these files to swap creds.
	// Resolve the directory through paths.AccountDir rather than
	// hand-building it: the backup root differs per platform (XDG on
	// Linux, ~/.cux elsewhere) and a hardcoded layout only matched Linux.
	acct2Dir := paths.AccountDir(2, "free@x.test")
	if err := os.MkdirAll(acct2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCreds := `{"cux-test-slot":"2","claudeAiOauth":{"accessToken":"test-token-2"}}`
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
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
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

// agedAccountUsage is hookAccountUsage with a poll time, so a test can build
// the reading a frozen cache actually holds.
func agedAccountUsage(five, seven float64, age time.Duration) usage.AccountUsage {
	u := hookAccountUsage(five, seven)
	u.PolledAt = time.Now().UTC().Add(-age)
	return u
}

// TestRenderPromptUsageMarksAStaleCacheUnknown reproduces issue #46 on the
// exact surface that hid it: the slash-command output, with no refresh, over
// a cache twelve days old. Every figure there is a claim about now, and none
// of them is supported.
func TestRenderPromptUsageMarksAStaleCacheUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 2,
		Sequence:   []int{1, 2, 3},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "cancelled@x.test"},
			2: {Slot: 2, Email: "active@x.test"},
			3: {Slot: 3, Email: "other@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	const stale = 307 * time.Hour // 12.8 days, as reported
	if err := usage.SaveCache(usage.Cache{
		"cancelled@x.test": agedAccountUsage(0, 23, stale),
		"active@x.test":    agedAccountUsage(0, 0, stale),
		"other@x.test":     agedAccountUsage(4, 42, stale),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := renderPromptUsage(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "USAGE DATA STALE") {
		t.Fatalf("no staleness warning on the slash-command surface:\n%s", out)
	}
	if !strings.Contains(out, "12.8 D") {
		t.Fatalf("warning did not report the age of the reading:\n%s", out)
	}
	// The frozen percentages must not be rendered as if measured.
	for _, pct := range []string{"23%", "42%"} {
		if strings.Contains(out, pct) {
			t.Fatalf("stale percentage %s still rendered as a live figure:\n%s", pct, out)
		}
	}
	// And no account may be advertised as the one to move to.
	if strings.Contains(out, "NEXT USABLE") {
		t.Fatalf("a stale reading was used to recommend an account:\n%s", out)
	}
	if !strings.Contains(out, "STALE") {
		t.Fatalf("rows did not carry a stale state label:\n%s", out)
	}
}

// TestRenderPromptUsageLeavesAFreshCacheAlone is the other half: the
// staleness plumbing must be invisible when polling is working.
func TestRenderPromptUsageLeavesAFreshCacheAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 1,
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
		"a@x.test": agedAccountUsage(10, 20, time.Minute),
		"b@x.test": agedAccountUsage(5, 15, time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := renderPromptUsage(false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "USAGE DATA STALE") {
		t.Fatalf("fresh cache reported as stale:\n%s", out)
	}
	if !strings.Contains(out, "NEXT USABLE") {
		t.Fatalf("fresh cache should still recommend an account:\n%s", out)
	}
	if !strings.Contains(out, "20%") {
		t.Fatalf("fresh figures should render as measured:\n%s", out)
	}
}

// TestPromptSwitchHasTargetDoesNotBlockOnAStaleCache is the #37 failure mode
// reached through issue #46: this function can refuse the user's prompt with
// "all managed accounts are exhausted", and a frozen cache must never be what
// produces that verdict.
func TestPromptSwitchHasTargetDoesNotBlockOnAStaleCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 1,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	// Both accounts look completely exhausted — on readings 12.8 days old.
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": agedAccountUsage(100, 100, 307*time.Hour),
		"b@x.test": agedAccountUsage(100, 100, 307*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if ok, msg := promptSwitchHasTarget(); !ok {
		t.Fatalf("a stale cache blocked the prompt: %s", msg)
	}
}

// TestPromptSwitchHasTargetStillBlocksOnAFreshExhaustedPool is the control:
// failing open on stale data must not mean never failing at all.
func TestPromptSwitchHasTargetStillBlocksOnAFreshExhaustedPool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", t.TempDir()+"/config.json")

	state := &store.State{
		ActiveSlot: 1,
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
		"a@x.test": agedAccountUsage(100, 100, time.Minute),
		"b@x.test": agedAccountUsage(100, 100, time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := promptSwitchHasTarget(); ok {
		t.Fatal("a genuinely exhausted pool should still block the prompt")
	}
}
