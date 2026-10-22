package wrapper

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/usage"
)

// idleStubChild stands in for the claude process; step() only reaches it when
// a swap is being carried out.
type idleStubChild struct{}

func (idleStubChild) Pid() int               { return 0 }
func (idleStubChild) Signal(os.Signal) error { return nil }
func (idleStubChild) Kill() error            { return nil }
func (idleStubChild) Exited() bool           { return true }
func (idleStubChild) Wait() error            { return nil }

func writeTranscript(t *testing.T, dir, sid string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionHasUserMessage(t *testing.T) {
	const cwd = "/Users/x/work/my.project_1"
	cases := []struct {
		name  string
		lines []string // nil: no transcript file at all
		want  bool
	}{
		{name: "no transcript file", lines: nil, want: false},
		{name: "empty file", lines: []string{}, want: false},
		{
			name: "only bookkeeping lines",
			lines: []string{
				`{"type":"file-history-snapshot","snapshot":{}}`,
				`{"type":"summary","summary":"x"}`,
				`{"type":"system","content":"hook output"}`,
			},
			want: false,
		},
		{
			name:  "user line that is injected meta, not typed",
			lines: []string{`{"type":"user","isMeta":true,"message":{"role":"user","content":"Caveat: ..."}}`},
			want:  false,
		},
		{
			name:  "a typed prompt",
			lines: []string{`{"type":"file-history-snapshot"}`, `{"type":"user","message":{"role":"user","content":"fix the bug"}}`},
			want:  true,
		},
		{
			name:  "a typed prompt as content parts",
			lines: []string{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`},
			want:  true,
		},
		{
			name:  "a slash command counts as input",
			lines: []string{`{"type":"user","message":{"role":"user","content":"<command-name>/model</command-name>"}}`},
			want:  true,
		},
		{
			name:  "a truncated last line is not a message",
			lines: []string{`{"type":"summary"}`, `{"type":"user","message":{"role":"us`},
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, ".claude"))
			if c.lines != nil {
				// Claude Code's own encoding: '.' and '_' fold to '-' as well.
				writeTranscript(t, filepath.Join(paths.ClaudeProjectsDir(), "-Users-x-work-my-project-1"), "s-1", c.lines...)
			}
			if got := sessionHasUserMessage(cwd, "s-1"); got != c.want {
				t.Errorf("sessionHasUserMessage = %v, want %v", got, c.want)
			}
		})
	}

	t.Run("empty session id", func(t *testing.T) {
		if sessionHasUserMessage(cwd, "") {
			t.Error("an unknown session id read as having a message")
		}
	})
	t.Run("transcript under cux's slash-only encoding is found too", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, ".claude"))
		writeTranscript(t, paths.ProjectTranscriptDir("/Users/x/plain"), "s-2",
			`{"type":"user","message":{"role":"user","content":"hello"}}`)
		if !sessionHasUserMessage("/Users/x/plain", "s-2") {
			t.Error("transcript at the ProjectTranscriptDir path was not found")
		}
	})
}

// stepIdle runs one step() tick for a wrapper whose idle window has long
// passed, on a seat that is over threshold with a free seat available — the
// exact situation that triggers an idle migration — and returns what the
// tick did.
func stepIdle(t *testing.T, transcript []string) (swap *pending, stopped bool, out string) {
	t.Helper()
	accounts := threeSeats()
	swapFixture(t, accounts, usage.Cache{
		"live@x.test": freshUsage(100, 100),
		"b@x.test":    freshUsage(10, 10),
		"c@x.test":    freshUsage(10, 10),
	})
	tmp := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, ".claude"))
	// Never let a test reach the usage endpoint: seats are fresh so nothing
	// should poll, and this makes a regression fail loudly instead of online.
	t.Setenv("CUX_USAGE_ENDPOINT", "http://127.0.0.1:1")

	cwd, _ := os.Getwd()
	if transcript != nil {
		writeTranscript(t, paths.ProjectTranscriptDir(cwd), "s-idle", transcript...)
	}

	cfg := config.Defaults()
	cfg.AutoSwitchOnThreshold = true
	cfg.AutoResume = true
	cfg.AutoSwapIdleAfterSeconds = 60

	var (
		mu       sync.Mutex
		sid      = "s-idle"
		stopReq  atomic.Bool
		hadTurns atomic.Bool
		buf      bytes.Buffer
	)
	act := newActivity(time.Now().Add(-time.Hour))
	act.nextCheck = time.Now().Add(-time.Minute)
	step(4242431, &cfg, "", &mu, &sid, &swap, &stopReq, &hadTurns, act, &parkState{}, &idleStubChild{}, &buf)
	return swap, stopReq.Load(), buf.String()
}

// The ghost-session fix. A window with no user message has nothing to
// resume, so migrating it only opens an empty session next to it.
func TestIdleSwapSkipsASessionWithNoUserMessage(t *testing.T) {
	for name, transcript := range map[string][]string{
		"no transcript at all": nil,
		"bookkeeping only":     {`{"type":"file-history-snapshot","snapshot":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			swap, stopped, out := stepIdle(t, transcript)
			if swap != nil || stopped || strings.Contains(out, "migrating") {
				t.Fatalf("idle swap ran for a session nobody typed into: swap=%v stopped=%v out=%q", swap, stopped, out)
			}
		})
	}
}

// The control. Without it the test above would also pass if the fixture
// simply could not produce an idle swap.
func TestIdleSwapStillRunsForASessionWithAUserMessage(t *testing.T) {
	swap, stopped, out := stepIdle(t, []string{
		`{"type":"user","message":{"role":"user","content":"refactor the parser"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"done"}}`,
	})
	if swap == nil || !swap.idle {
		t.Fatalf("idle swap did not run for a session with a user message (swap=%+v out=%q)", swap, out)
	}
	if !stopped || !strings.Contains(out, "migrating now") {
		t.Fatalf("idle swap was not carried out: stopped=%v out=%q", stopped, out)
	}
}
