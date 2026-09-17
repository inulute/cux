package wrapper

// Keeping a session open when there is nowhere to switch to.
//
// A rate limit used to stop claude first and only afterwards ask whether any
// seat could take the session. With every seat exhausted the answer was "no",
// so the process had been killed for nothing: the conversation came back via
// --resume after the reset, but the prompt line was gone for hours and every
// background subagent died with it. In the worst case the seat that "reaches
// its reset first" was the live seat itself (#48, #50).
//
// The decision now happens before the child is touched. For a session
// someone comes back to, that is the whole change: credentials are global and
// claude re-reads them per request, so it picks up whichever seat is live
// when the user next types — and if that seat is still out, the
// UserPromptSubmit hook swaps in place right then (hooks.go, inPlaceSwap).
//
// A session nobody comes back to still needs the old path. auto_message can
// only be delivered as an argv on a fresh process, so once a seat frees up
// and no prompt has arrived since the park, the session is handed to the
// normal stop-swap-resume so the work actually continues.

import (
	"fmt"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

// parkCheckInterval is how often a parked session that nobody has come back
// to re-checks. Reset clocks are minutes to days away.
const parkCheckInterval = time.Minute

// parkState is owned by the single poll goroutine that runs step().
type parkState struct {
	p    *pending
	next time.Time
	// typed is set by the prompt that arrives while parked. It is the whole
	// "is anyone there" test: act.lastAt cannot answer it, because a turn
	// killed by a limit reports StopFailure rather than Stop and so never
	// advances it.
	typed bool
}

func (s *parkState) start(p *pending, now time.Time) {
	s.p, s.next, s.typed = p, now.Add(parkCheckInterval), false
}

// Seams for tests.
var (
	parkRefresh  = func() { _, _ = monitor.RefreshAll() }
	parkResolve  = resolveTarget
	parkLiveRoom = func(cfg *config.Config) bool { _, ok := liveAccountWithCapacity(cfg); return ok }
)

// shouldPark reports whether a rate-limit swap has nowhere to go, in which
// case claude must not be stopped for it. Callers refresh first: the reading
// has to postdate the limit, or a sibling's from seconds earlier reads as room.
func shouldPark(cfg *config.Config, p *pending) bool {
	if cfg == nil || p == nil || p.retryOnly || p.trigger != history.TriggerRateLimit {
		return false
	}
	// A model-specific cap is answered by relaunching on another model on the
	// same seat (#57): that needs a restart and has somewhere to go.
	if p.refusedModel != "" && len(cfg.ModelFallback) > 0 {
		return false
	}
	if _, err := parkResolve(p.explicitTarget, p.trigger, cfg, nil); err == nil {
		return false
	}
	// The live seat still has room by its numbers — a transient 429 or a
	// sub-cap. completeSwap retries in place on a backoff, which is what
	// keeps unattended sessions moving; that path is left alone.
	return !parkLiveRoom(cfg)
}

// markParked tells `cux sessions` this one is alive but waiting. Nothing is
// printed: claude owns the screen and already shows its own limit notice.
func markParked(cfg *config.Config) {
	detail := "keeping this session open; no seat has room"
	if state, err := store.Load(); err == nil {
		cache, _ := usage.LoadCache()
		if readyAt, email, ok := nextAvailability(state.PoolForCwd(), cache, cfg.Thresholds, time.Now()); ok {
			detail = fmt.Sprintf("keeping this session open; %s frees up first at %s",
				email, readyAt.Local().Format("15:04"))
		}
	}
	registry.UpdateSelf(func(e *registry.Entry) {
		e.State = registry.StateParked
		e.Detail = detail
	})
}

// parkResumeTarget reports how a parked session that nobody returned to
// should be resumed now that a seat is usable, or nil to keep waiting.
func parkResumeTarget(cfg *config.Config, p *pending) *pending {
	if _, err := parkResolve(p.explicitTarget, p.trigger, cfg, nil); err == nil {
		// Re-read the seat we are leaving: the park may be hours old, and
		// this snapshot is what `cux history` records as the "from" usage
		// and what skipSwapOnCapacity compares against.
		p.fromUsage = snapshotActiveUsage()
		p.fromKey, _ = switcher.CurrentLiveCacheKey()
		return p
	}
	if parkLiveRoom(cfg) {
		// Only the live seat came back: relaunch on it rather than swapping.
		return &pending{retryOnly: true, reason: p.reason}
	}
	return nil
}
