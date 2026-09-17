package wrapper

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/signals"
)

// Keeping a session open when no seat has room (#48 item 1, #50).
//
// The failure these guard against: a rate-limit signal stopped claude first
// and asked whether any seat could take the session afterwards. With every
// seat exhausted that killed the prompt line and every running subagent for
// nothing - and when the live seat was the one that reset first, the restart
// could not even move the session anywhere.

type recordingChild struct {
	mu      sync.Mutex
	signals []os.Signal
	kills   int
}

// Pid 0 on purpose: gracefulExit collects and kills the descendants of the
// child's PID, and an invented number could be a real process on the test
// machine. With 0 it returns at once; the stop is measured via stopRequested.
func (c *recordingChild) Pid() int { return 0 }
func (c *recordingChild) Signal(s os.Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals = append(c.signals, s)
	return nil
}
func (c *recordingChild) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kills++
	return nil
}
func (c *recordingChild) Exited() bool { return true }
func (c *recordingChild) Wait() error  { return nil }
func (c *recordingChild) stopAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.signals) + c.kills
}

type parkHarness struct {
	t        *testing.T
	cfg      config.Config
	pid      int
	mu       sync.Mutex
	session  string
	swap     *pending
	stop     atomic.Bool
	hadTurns atomic.Bool
	act      *activity
	park     *parkState
	child    *recordingChild

	target   bool // does resolveTarget find a seat?
	liveRoom bool // does the live seat have room?
	now      time.Time
}

func newParkHarness(t *testing.T) *parkHarness {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))

	h := &parkHarness{
		t:     t,
		cfg:   config.Defaults(),
		pid:   424243,
		act:   newActivity(time.Now()),
		park:  &parkState{},
		child: &recordingChild{},
		now:   time.Now(),
	}
	realRefresh, realRefreshNow, realResolve, realRoom, realNow := parkRefresh, parkRefreshNow, parkResolve, parkLiveHasRoom, parkNow
	t.Cleanup(func() {
		parkRefresh, parkRefreshNow, parkResolve, parkLiveHasRoom, parkNow = realRefresh, realRefreshNow, realResolve, realRoom, realNow
	})
	parkRefresh = func() {}
	parkRefreshNow = func() {}
	parkResolve = func(explicit string, _ history.Trigger, _ *config.Config, _ map[int]bool) (string, error) {
		if explicit != "" {
			return explicit, nil
		}
		if h.target {
			return "2", nil
		}
		return "", errors.New("no usable accounts available")
	}
	parkLiveHasRoom = func(*config.Config, time.Time) bool { return h.liveRoom }
	parkNow = func() time.Time { return h.now }
	return h
}

func (h *parkHarness) send(name signals.Name, payload interface{}) {
	h.t.Helper()
	if err := signals.Write(h.pid, name, payload); err != nil {
		h.t.Fatal(err)
	}
}

func (h *parkHarness) step() {
	step(h.pid, &h.cfg, "", &h.mu, &h.session, &h.swap, &h.stop, &h.hadTurns, h.act, h.park, h.child, io.Discard)
	// gracefulExit runs in a goroutine; give it the chance to act.
	time.Sleep(50 * time.Millisecond)
}

func (h *parkHarness) stopped() bool { return h.stop.Load() || h.child.stopAttempts() > 0 }

func (h *parkHarness) rateLimit() {
	h.send(signals.RateLimited, signals.RateLimitedPayload{Timestamp: time.Now(), Message: "You've reached your session limit"})
}

func (h *parkHarness) laterByAMinute() { h.now = h.now.Add(parkCheckInterval + time.Second) }

func TestRateLimitWithNowhereToGoKeepsClaudeRunning(t *testing.T) {
	h := newParkHarness(t)
	h.rateLimit()
	h.step()

	if h.stopped() {
		t.Fatal("claude was stopped although no seat had room")
	}
	if h.swap != nil {
		t.Fatalf("a swap was queued with nowhere to go: %+v", h.swap)
	}
	if !h.park.active() {
		t.Fatal("the session was not parked")
	}
	b, err := os.ReadFile(parkLogFile())
	if err != nil || !strings.Contains(string(b), `"event":"park"`) {
		t.Fatalf("park decision not logged: %v %s", err, b)
	}
}

// Control: with a seat to go to, the upstream behaviour is untouched.
func TestRateLimitWithATargetStillStopsClaude(t *testing.T) {
	h := newParkHarness(t)
	h.target = true
	h.rateLimit()
	h.step()

	if !h.stopped() {
		t.Fatal("claude was not stopped although a seat had room")
	}
	if h.swap == nil || h.swap.trigger != history.TriggerRateLimit {
		t.Fatalf("expected a rate-limit swap, got %+v", h.swap)
	}
	if h.park.active() {
		t.Fatal("parked although a target existed")
	}
}

// A transient 429 on a seat that still has room keeps the in-place retry,
// which is what lets unattended sessions continue by themselves.
func TestRateLimitOnASeatWithRoomKeepsTheInPlaceRetry(t *testing.T) {
	h := newParkHarness(t)
	h.liveRoom = true
	h.rateLimit()
	h.step()

	if !h.stopped() || h.swap == nil {
		t.Fatal("a seat with room should still go through the in-place retry")
	}
}

func TestSwitchOffRestoresStopFirst(t *testing.T) {
	h := newParkHarness(t)
	h.cfg.KeepSessionWhenExhausted = false
	h.rateLimit()
	h.step()

	if !h.stopped() || h.park.active() {
		t.Fatal("keep_session_when_exhausted=false must restore the upstream behaviour")
	}
}

func TestParkedSessionComesBackWithoutRestartWhenTheLiveSeatResetsFirst(t *testing.T) {
	h := newParkHarness(t)
	h.rateLimit()
	h.step()
	if !h.park.active() {
		t.Fatal("precondition: parked")
	}

	// Before the check interval nothing is evaluated.
	h.liveRoom = true
	h.step()
	if !h.park.active() {
		t.Fatal("re-evaluated before the check interval")
	}

	h.laterByAMinute()
	h.step()
	if h.park.active() {
		t.Fatal("still parked although the live seat has room again")
	}
	if h.stopped() || h.swap != nil {
		t.Fatal("the live seat came back first - no restart is needed, but claude was stopped")
	}
}

func TestParkedSessionMovesOnlyWhenNoTurnIsInFlight(t *testing.T) {
	h := newParkHarness(t)
	h.rateLimit()
	h.step()

	// The user keeps typing while parked.
	h.send(signals.PromptSubmitted, signals.PromptSubmittedPayload{Timestamp: time.Now(), StartsTurn: true})
	h.step()

	// Another seat frees up mid-turn: wait.
	h.target = true
	h.laterByAMinute()
	h.step()
	if h.stopped() {
		t.Fatal("a turn was in flight, but the parked session was moved")
	}
	if !h.park.active() {
		t.Fatal("left the parked state while the user was busy")
	}

	// The turn ends: now it may move.
	h.send(signals.Stopped, signals.StoppedPayload{Timestamp: time.Now()})
	h.laterByAMinute()
	h.step()
	if !h.stopped() {
		t.Fatal("a seat is free and the session is idle, but it was not moved")
	}
	if h.swap == nil || h.swap.trigger != history.TriggerRateLimit || !strings.Contains(h.swap.reason, "kept open") {
		t.Fatalf("expected the parked rate-limit decision to be handed on, got %+v", h.swap)
	}
	if h.park.active() {
		t.Fatal("park not cleared after handing on")
	}
}

func TestLiveSeatBackWinsOverAnotherSeatFreeingUp(t *testing.T) {
	h := newParkHarness(t)
	h.rateLimit()
	h.step()
	h.target, h.liveRoom = true, true
	h.laterByAMinute()
	h.step()
	if h.stopped() {
		t.Fatal("moved to another seat although the live seat was usable again")
	}
}

func TestPlainSwitchWithNowhereToGoDoesNotStopClaude(t *testing.T) {
	h := newParkHarness(t)
	h.send(signals.SwitchRequested, signals.SwitchRequestedPayload{Timestamp: time.Now()})
	h.step()
	if h.stopped() || h.swap != nil {
		t.Fatal("a /switch with nowhere to go restarted claude on the same seat")
	}
}

func TestSwitchWithAnExplicitTargetIsHonoured(t *testing.T) {
	h := newParkHarness(t)
	h.send(signals.SwitchRequested, signals.SwitchRequestedPayload{Timestamp: time.Now(), Target: "3"})
	h.step()
	if !h.stopped() || h.swap == nil || h.swap.explicitTarget != "3" {
		t.Fatalf("a named target must always be honoured, got %+v", h.swap)
	}
}

// A prompt the threshold hook intercepted must reach the model somewhere, so
// that path is never swallowed.
func TestInterceptedPromptSwitchIsNeverSwallowed(t *testing.T) {
	h := newParkHarness(t)
	h.send(signals.SwitchRequested, signals.SwitchRequestedPayload{Timestamp: time.Now(), ResumeMessage: "weiter"})
	h.step()
	if !h.stopped() {
		t.Fatal("the intercepted prompt would be lost")
	}
}

func TestShouldParkDecisionTable(t *testing.T) {
	newParkHarness(t)
	cfg := config.Defaults()
	rl := func() *pending { return &pending{trigger: history.TriggerRateLimit, reason: "limit"} }

	parkLiveHasRoom = func(*config.Config, time.Time) bool { return false }
	parkResolve = func(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
		return "", errors.New("none")
	}
	if !shouldPark(&cfg, rl()) {
		t.Error("exhausted pool: want park")
	}
	if shouldPark(&cfg, &pending{retryOnly: true}) {
		t.Error("retry-only must not park")
	}
	if shouldPark(&cfg, &pending{trigger: history.TriggerThreshold}) {
		t.Error("threshold swaps carry their own target and must not park")
	}
	withModel := rl()
	withModel.refusedModel = "fable"
	cfgModel := config.Defaults()
	cfgModel.ModelFallback = []string{"opus"}
	if shouldPark(&cfgModel, withModel) {
		t.Error("a model cap with a fallback chain relaunches on another model and must not park")
	}
	if !shouldPark(&cfg, withModel) {
		t.Error("a model cap without a fallback chain is an ordinary exhausted pool")
	}
}
