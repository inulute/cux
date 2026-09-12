package wrapper

import (
	"fmt"
	"strconv"

	"github.com/inulute/cux/internal/config"
)

// Claude Code environment flags that quietly disable cux's reactive path.
//
// auto_switch_on_rate_limit is built on StopFailure: claude gives up on a
// turn, the hook fires, cux swaps the seat. Anything that stops claude from
// giving up therefore stops the swap — not by breaking it, but by removing
// the event it waits for. The session sits out the full window with a free
// seat in the pool, and every surface cux has still reports a healthy
// install, because it is one (#56).
//
// Nobody sets these flags expecting that. They set them so a transient 429
// does not end a long unattended run, which is a good reason. The trade is
// only invisible because nothing names it.
const (
	envRetryWatchdog = "CLAUDE_CODE_RETRY_WATCHDOG"
	envMaxRetries    = "CLAUDE_CODE_MAX_RETRIES"

	// maxRetriesNotable is where a retry ladder gets long enough to cover a
	// rate-limit window rather than a blip. Below it the turn still fails
	// soon enough for the swap to be the faster way back.
	maxRetriesNotable = 10
)

// claudeEnvWarnings reports the Claude Code env flags in effect that
// suppress the rate-limit swap. getenv is injected so the check is testable
// without touching the process environment.
func claudeEnvWarnings(cfg *config.Config, getenv func(string) string) []string {
	// Only the reactive path depends on StopFailure. With
	// auto_switch_on_rate_limit off there is nothing to suppress, and the
	// warning would be noise on every launch.
	if cfg == nil || !cfg.AutoSwitchOnRateLimit {
		return nil
	}

	var out []string
	// Claude Code reads this flag for truthiness in JavaScript, where every
	// non-empty string qualifies — "0" and "false" included. Matching that
	// exactly matters more than guessing at intent: a user who set it to "0"
	// believing it off has the watchdog on, which is precisely the case worth
	// telling them about.
	if getenv(envRetryWatchdog) != "" {
		out = append(out, fmt.Sprintf(
			"%s is set: claude waits out rate limits instead of failing the turn, so no "+
				"StopFailure is emitted and auto_switch_on_rate_limit cannot fire.", envRetryWatchdog))
	}
	if n, err := strconv.Atoi(getenv(envMaxRetries)); err == nil && n >= maxRetriesNotable {
		out = append(out, fmt.Sprintf(
			"%s=%d: claude retries for a long time before the turn fails, delaying the "+
				"rate-limit swap by roughly that much.", envMaxRetries, n))
	}
	return out
}
