package wrapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

func TestResolveTargetDoesNotRotateToWeeklyFullFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

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
		"a@x.test": accountUsage(94, 67),
		"b@x.test": accountUsage(0, 100),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	_, err := resolveTarget("", history.TriggerManual, &cfg)
	if err == nil {
		t.Fatal("resolveTarget should refuse to rotate when every alternate account is exhausted")
	}
	if !strings.Contains(err.Error(), "no usable accounts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRotateFallbackAllowsMissingUsage(t *testing.T) {
	state := &store.State{
		ActiveSlot: 1,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	cfg := config.Defaults()
	got, err := rotateFallback(state, usage.Cache{}, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2" {
		t.Fatalf("rotateFallback = %q, want slot 2", got)
	}
}

func TestShouldPreflightHardLimitOnlyForResume(t *testing.T) {
	if !shouldPreflightHardLimit([]string{"--resume", "session-id"}) {
		t.Fatal("expected --resume to enable hard-limit preflight")
	}
	if !shouldPreflightHardLimit([]string{"-r", "session-id"}) {
		t.Fatal("expected -r to enable hard-limit preflight")
	}
	if shouldPreflightHardLimit([]string{"--model", "sonnet"}) {
		t.Fatal("non-resume launch should not preflight hard-limit switching")
	}
}

func TestEvaluatePrelaunchHardLimitSwapUsesRateLimitTrigger(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claudeDir, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"a@x.test","accountUuid":"u1"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

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
		"a@x.test": accountUsage(100, 67),
		"b@x.test": accountUsage(0, 20),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	p, blocked := evaluatePrelaunchHardLimitSwap(&cfg)
	if blocked != "" {
		t.Fatalf("unexpected blocked message: %s", blocked)
	}
	if p == nil {
		t.Fatal("expected hard-limited active account to preflight swap")
	}
	if p.trigger != history.TriggerRateLimit {
		t.Fatalf("trigger = %q, want %q", p.trigger, history.TriggerRateLimit)
	}
	// Picks now carry the seat's slot number — unambiguous where emails
	// are not (the same email can hold personal + org seats).
	if p.explicitTarget != "2" {
		t.Fatalf("target = %q, want slot \"2\" (b@x.test)", p.explicitTarget)
	}
	if !strings.Contains(p.reason, "before launch") {
		t.Fatalf("reason should include before launch, got %q", p.reason)
	}
}

func TestEvaluatePrelaunchHardLimitSwapBlocksWhenAllAccountsFull(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claudeDir, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"a@x.test","accountUuid":"u1","organizationUuid":"org-live"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

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
		"a@x.test": accountUsage(100, 67),
		"b@x.test": accountUsage(100, 20),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	p, blocked := evaluatePrelaunchHardLimitSwap(&cfg)
	if p != nil {
		t.Fatalf("expected no target when all accounts are full, got %#v", p)
	}
	if !strings.Contains(blocked, "all managed accounts are exhausted") {
		t.Fatalf("blocked message should explain exhaustion, got: %s", blocked)
	}
	if !strings.Contains(blocked, "Next available account") {
		t.Fatalf("blocked message should include next available account, got: %s", blocked)
	}
}

func accountUsage(five, seven float64) usage.AccountUsage {
	w5 := usage.Window{Utilization: five}
	w7 := usage.Window{Utilization: seven}
	return usage.AccountUsage{FiveHour: &w5, SevenDay: &w7}
}

func TestIsResumeArgv(t *testing.T) {
	if !isResumeArgv([]string{"--resume", "abc"}) {
		t.Fatal("expected --resume argv")
	}
	if !isResumeArgv([]string{"-r", "abc"}) {
		t.Fatal("expected -r argv")
	}
	if isResumeArgv([]string{"--model", "sonnet"}) {
		t.Fatal("non-resume argv should return false")
	}
}

func TestResumeSessionID(t *testing.T) {
	if got := resumeSessionID([]string{"--resume", "sess-1", "hello"}); got != "sess-1" {
		t.Fatalf("resumeSessionID = %q, want sess-1", got)
	}
	if got := resumeSessionID([]string{"-r", "sess-2"}); got != "sess-2" {
		t.Fatalf("resumeSessionID = %q, want sess-2", got)
	}
}

func TestWaitForTranscriptStableFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := "test-session"
	path := filepath.Join(paths.ProjectTranscriptDir(cwd), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForTranscript(cwd, sessionID, time.Second) {
		t.Fatal("waitForTranscript should succeed for a stable non-empty file")
	}
}

func TestWaitForTranscriptEventuallyStable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := "growing-session"
	path := filepath.Join(paths.ProjectTranscriptDir(cwd), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			_ = os.WriteFile(path, []byte(strings.Repeat("x", (i+1)*20)), 0o600)
			time.Sleep(80 * time.Millisecond)
		}
	}()

	if !waitForTranscript(cwd, sessionID, 2*time.Second) {
		t.Fatal("waitForTranscript should succeed once writes stop")
	}
	<-done
}

func TestWaitForTranscriptMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if waitForTranscript(cwd, "missing-session", 150*time.Millisecond) {
		t.Fatal("waitForTranscript should return false when file never appears")
	}
}
