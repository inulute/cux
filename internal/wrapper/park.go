package wrapper

// Keeping a session open when there is nowhere to switch to.
//
// Before this, a rate-limit signal stopped claude at once (step, section 2)
// and only afterwards, in the main loop, asked whether any seat could take
// the session. With every seat exhausted the answer was "no", so the process
// had been killed for nothing: the conversation came back via --resume after
// the reset, but the prompt line was gone for hours and every background
// subagent the session was running died with it (#48 item 1, #50). In the
// worst case the seat that "reaches its reset first" was the live seat
// itself, so the restart could not even move the session anywhere.
//
// Now the decision is made before the child is touched:
//
//   - a target exists            -> stop and swap, exactly as before;
//   - the live seat still has
//     room by its numbers        -> stop and retry in place, as before (keeps
//                                   unattended auto-continue on transient 429s);
//   - nothing usable at all      -> park: claude keeps running, the user can
//                                   keep typing, nothing is terminated.
//
// While parked the wrapper re-checks once a minute:
//
//   - the live seat has room again -> unpark, no restart at all;
//   - another seat has room and no
//     turn is in flight            -> hand the parked decision to the normal
//                                     swap path (stop, swap, --resume).
//
// Switched off with `cux config set keep_session_when_exhausted false`, which
// restores the upstream behaviour without swapping binaries.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

// parkCheckInterval is how often a parked session re-evaluates. Reset clocks
// are minutes to days away; a minute of latency on noticing one is invisible.
const parkCheckInterval = time.Minute

// parkState is owned by the poll goroutine of one launch; nothing else reads
// or writes it, so it needs no lock of its own.
type parkState struct {
	p         *pending
	since     time.Time
	nextCheck time.Time
	// userBusy is set when a prompt that starts a turn is submitted while
	// parked, and cleared by the Stop (or the next limit signal) that ends
	// it. A parked session is only ever moved while it is false.
	userBusy bool
}

func (s *parkState) active() bool { return s != nil && s.p != nil }

func (s *parkState) start(p *pending, now time.Time) {
	s.p = p
	s.since = now
	s.nextCheck = now.Add(parkCheckInterval)
	s.userBusy = false
}

func (s *parkState) clear() {
	s.p = nil
	s.userBusy = false
}

type parkVerdict int

const (
	parkWait parkVerdict = iota
	parkLiveBack
	parkMigrate
)

// Seams for tests: the decisions below are pure functions of these.
var (
	parkRefresh = func() { _, _ = monitor.RefreshAllCoalesced(refreshCoalesceWindow) }
	parkResolve = resolveTarget
	// parkLiveHasRoom reports whether the live seat has capacity on a reading
	// taken after `after` (zero time: any fresh reading).
	parkLiveHasRoom = liveSeatHasRoomSince
	parkNow         = time.Now
)

// HasRotationTarget reports whether a plain rotation (no explicit target)
// would find a seat right now. The /switch hook uses it so the precheck and
// the wrapper cannot disagree: the hook used to let a switch through on a
// looser capacity rule, and the wrapper then restarted claude on the very
// same seat.
func HasRotationTarget(cfg *config.Config) bool {
	_, err := resolveTarget("", history.TriggerManual, cfg, nil)
	return err == nil
}

// shouldPark decides, before the child is stopped, whether a pending swap has
// anywhere to go. true means: do not stop claude.
func shouldPark(cfg *config.Config, p *pending) bool {
	if cfg == nil || !cfg.KeepSessionWhenExhausted || p == nil || p.retryOnly {
		return false
	}
	if p.trigger != history.TriggerRateLimit {
		// Threshold and idle swaps only fire with a target already picked,
		// and a manual /switch is handled by shouldIgnoreManualSwitch.
		return false
	}
	// A model-specific cap is answered by relaunching on another model on the
	// same seat (#57) - that needs a restart and has somewhere to go.
	if p.refusedModel != "" && len(cfg.ModelFallback) > 0 {
		return false
	}
	parkRefresh()
	if _, err := parkResolve(p.explicitTarget, p.trigger, cfg, nil); err == nil {
		return false
	}
	// The seat that hit the limit still has room by its numbers: a transient
	// 429 or a sub-cap. completeSwap retries in place on a backoff, which is
	// what keeps unattended sessions moving; that path is left alone.
	if parkLiveHasRoom(cfg, time.Time{}) {
		return false
	}
	return true
}

// shouldIgnoreManualSwitch reports whether a plain /switch (no target, not a
// prompt intercepted by the threshold hook) has nowhere to go. Stopping claude
// for it only restarts the session on the same seat.
func shouldIgnoreManualSwitch(cfg *config.Config, p *pending) bool {
	if cfg == nil || !cfg.KeepSessionWhenExhausted || p == nil {
		return false
	}
	if p.trigger != history.TriggerManual || p.explicitTarget != "" || p.resumeMessage != "" {
		return false
	}
	parkRefresh()
	_, err := parkResolve("", history.TriggerManual, cfg, nil)
	return err != nil
}

// evaluatePark is the once-a-minute check for a parked session.
func evaluatePark(cfg *config.Config, s *parkState) parkVerdict {
	if !s.active() {
		return parkWait
	}
	parkRefresh()
	if parkLiveHasRoom(cfg, s.since) {
		return parkLiveBack
	}
	if _, err := parkResolve(s.p.explicitTarget, s.p.trigger, cfg, nil); err == nil {
		if s.userBusy {
			return parkWait
		}
		return parkMigrate
	}
	return parkWait
}

// liveSeatHasRoomSince is liveAccountWithCapacity, additionally requiring the
// reading to postdate `after`. A reading from before the limit was hit cannot
// say the limit is over.
func liveSeatHasRoomSince(cfg *config.Config, after time.Time) bool {
	acct, ok := liveAccountWithCapacity(cfg)
	if !ok {
		return false
	}
	if after.IsZero() {
		return true
	}
	cache, _ := usage.LoadCache()
	u, found := cachedUsage(cache, acct.CacheKey(), acct.Email)
	return found && u.PolledAt.After(after) && !u.StaleReading(parkNow())
}

// parkDetail describes the wait for `cux sessions`.
func parkDetail(cfg *config.Config) string {
	state, err := store.Load()
	if err != nil {
		return "keeping this session open; no seat has room"
	}
	cache, _ := usage.LoadCache()
	if readyAt, email, ok := nextAvailability(state.PoolForCwd(), cache, cfg.Thresholds, parkNow()); ok {
		return fmt.Sprintf("keeping this session open; %s frees up first at %s", email, readyAt.Local().Format("15:04"))
	}
	return "keeping this session open; no seat has room, no reset clock known yet"
}

func markParked(cfg *config.Config) {
	detail := parkDetail(cfg)
	registry.UpdateSelf(func(e *registry.Entry) {
		e.State = registry.StateWaitingReset
		e.Detail = detail
	})
}

func markRunning() {
	registry.UpdateSelf(func(e *registry.Entry) {
		e.State = registry.StateRunning
		e.Detail = ""
	})
}

// parkLogFile records park decisions, so a real incident can be checked
// afterwards instead of guessed at. One JSON object per line.
func parkLogFile() string { return filepath.Join(paths.RuntimeDir(), "park-history.jsonl") }

func logPark(event string, p *pending, detail string) {
	entry := map[string]any{
		"timestamp": parkNow().UTC().Format(time.RFC3339),
		"pid":       os.Getpid(),
		"event":     event,
	}
	if seat, err := switcher.CurrentLiveEmail(); err == nil {
		entry["seat"] = seat
	}
	if p != nil {
		entry["trigger"] = string(p.trigger)
		entry["reason"] = snippet(p.reason)
	}
	if detail != "" {
		entry["detail"] = detail
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.MkdirAll(paths.RuntimeDir(), 0o700)
	f, err := os.OpenFile(parkLogFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
