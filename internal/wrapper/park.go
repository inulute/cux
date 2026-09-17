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
// The decision now happens before the child is touched. Nothing else is
// needed: credentials are global and claude re-reads them per request, so a
// session left running picks up whichever seat is live when the user next
// types — and if it is still exhausted, the UserPromptSubmit hook swaps in
// place at that moment (hooks.go, inPlaceSwap).

import (
	"fmt"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

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
		e.State = registry.StateWaitingReset
		e.Detail = detail
	})
}
