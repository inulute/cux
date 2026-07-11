// Package wrapper runs `claude` as a child process and orchestrates
// the v0.2 hook-driven swap flow:
//
//   - Claude Code's Stop / SessionStart / PostToolUseFailure hooks
//     write per-wrapper signal files via the `signals` package.
//   - The wrapper polls those signals while claude is alive.
//   - Threshold swaps wait for the next Stop signal, which fires only
//     after the turn's transcript has been flushed.
//   - Manual /switch and rate-limit swaps request a clean Claude exit
//     as soon as their signal is observed; at hard usage limits Claude
//     may not produce another Stop event.
//   - On exit the wrapper performs the swap and relaunches claude
//     with `--resume <session_id>`, so the conversation continues on
//     the new account.
//
// Stop gating remains the right behavior for proactive threshold swaps,
// while rate-limit and manual slash-command swaps must not depend on a
// future model turn that may never happen.
package wrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/strategy"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

const (
	envWrapped    = "CUX_WRAPPED"
	envWrapperPID = "CUX_WRAPPER_PID"
	envClaudeBin  = "CUX_CLAUDE_BIN"

	pollInterval           = 100 * time.Millisecond
	gracefulExitWait       = 5 * time.Second
	transcriptWaitTimeout  = 2 * time.Second
	transcriptStableWindow = 200 * time.Millisecond
	transcriptPollInterval = 50 * time.Millisecond
	resumeRetryExtraWait   = 500 * time.Millisecond
)

// pending captures the reason the wrapper has decided a swap is needed.
// trigger is the user-facing label that ends up in the swap history;
// explicitTarget is the target requested by /switch (empty otherwise,
// meaning "rotate per strategy"); fromUsage captures the active
// account's usage at the moment the swap was decided so the history
// entry can record it.
type pending struct {
	trigger        history.Trigger
	reason         string
	explicitTarget string
	resumeMessage  string
	fromUsage      usage.AccountUsage // best-effort snapshot
}

// Run executes the wrapper loop. argv is the args passed to `claude`
// on the first launch (anything after `cux`); subsequent launches are
// `--resume <session_id>` until a clean exit.
//
// claudeBin is the absolute path to the real `claude` binary.
func Run(claudeBin string, argv []string, w io.Writer) (int, error) {
	cfg, _ := config.Load() // garbage-config doesn't fail the wrapper; we just use defaults

	pid := os.Getpid()
	if err := os.MkdirAll(signals.Dir(), 0o700); err != nil {
		return 1, fmt.Errorf("wrapper: mkdir signals: %w", err)
	}
	// Wipe any leftover signals from a crashed prior run that
	// happened to have the same PID. PIDs recycle.
	signals.CleanupForPID(pid)
	defer signals.CleanupForPID(pid)
	defer os.Remove(paths.ReplayFlagFile(pid))
	if err := writeWrapperPID(pid); err != nil {
		fmt.Fprintf(w, "cux: warning: cannot publish wrapper pid: %v\n", err)
	}
	defer cleanupWrapperPID(pid)

	// lastManualTarget holds the email the user explicitly switched to
	// within this wrapper session. Threshold auto-switch is suppressed
	// while the live account matches this value, so a manual choice is
	// not silently undone by usage-based rotation.
	var lastManualTarget string

	currentArgv := argv
	if shouldPreflightHardLimit(argv) {
		_, _ = monitor.RefreshAll()
		if p, _ := evaluatePrelaunchHardLimitSwap(&cfg); p != nil {
			target, err := resolveTarget(p.explicitTarget, p.trigger, &cfg)
			if err != nil {
				fmt.Fprintf(w, "cux: %v — staying on current account\n", err)
			} else if from, to, err := switcher.SwitchTo(target); err != nil {
				fmt.Fprintf(w, "cux: prelaunch switch failed: %v\n", err)
			} else {
				fmt.Fprintf(w, "cux: %s on %s → switched to %s before resume\n", p.reason, from.Email, to.Email)
				lastManualTarget = ""
				setManualSwitchState("")
				appendPrelaunchHistory(from, to, p)
			}
		}
	} else {
		// One-shot background refresh so threshold checks have something
		// to work with on the first turn. Errors are ignored — a fresh
		// install with no usage data is fine; threshold logic falls back
		// to "no decision" rather than guessing.
		go func() { _, _ = monitor.RefreshAll() }()
	}

	var resumeRetryPending bool

	for {
		exitCode, sessionID, hadTurns, p, err := launch(claudeBin, currentArgv, pid, &cfg, lastManualTarget, w)
		if err != nil {
			return exitCode, err
		}

		if p == nil && resumeRetryPending && isResumeArgv(currentArgv) && exitCode != 0 {
			resumeRetryPending = false
			cwd, _ := os.Getwd()
			sid := resumeSessionID(currentArgv)
			if sid == "" {
				sid = sessionID
			}
			time.Sleep(resumeRetryExtraWait)
			waitForTranscript(cwd, sid, transcriptWaitTimeout)
			continue
		}
		resumeRetryPending = false

		if p == nil {
			// No swap pending ⇒ user quit normally.
			if shouldPreflightHardLimit(currentArgv) && isActiveHardLimited() {
				if msg := renderAllAccountsExhaustedMessage(&cfg); msg != "" {
					fmt.Fprintln(w, msg)
					fmt.Fprintln(w)
				}
			}
			// Print a cux-branded resume hint so the user knows to use
			// `cux --resume` (not `claude --resume`) to reconnect.
			if sessionID != "" {
				fmt.Fprintf(w, "cux --resume %s\n", sessionID)
			}
			return exitCode, nil
		}

		target, err := resolveTarget(p.explicitTarget, p.trigger, &cfg)
		if err != nil {
			if p.trigger == history.TriggerRateLimit || p.trigger == history.TriggerThreshold {
				_, _ = monitor.RefreshAll()
				target, err = resolveTarget(p.explicitTarget, p.trigger, &cfg)
			}
		}
		if err != nil {
			fmt.Fprintf(w, "cux: %v — staying on current account\n", err)
			return exitCode, nil
		}

		from, to, swapErr := switcher.SwitchTo(target)
		if swapErr != nil {
			fmt.Fprintf(w, "cux: switch failed: %v\n", swapErr)
			return 1, swapErr
		}

		// Append swap to the history log. Best-effort — a failure
		// here doesn't unwind the swap.
		cwd, _ := os.Getwd()
		toUsageCache, _ := usage.LoadCache()
		toUsage := toUsageCache[to.Email]
		entry := history.Entry{
			From:        from.Email,
			To:          to.Email,
			Trigger:     p.trigger,
			Reason:      p.reason,
			SessionID:   sessionID,
			CWD:         cwd,
			FromUsage5h: utilizationOrZero(p.fromUsage.FiveHour),
			FromUsage7d: utilizationOrZero(p.fromUsage.SevenDay),
			ToUsage5h:   utilizationOrZero(toUsage.FiveHour),
			ToUsage7d:   utilizationOrZero(toUsage.SevenDay),
		}
		_ = history.Append(entry)

		// Refresh both accounts' caches. The "from" entry now
		// represents the freshest reading we have; the "to" entry
		// will be updated again at the first Stop on the new
		// session, but doing it here gives `cux list` correct data
		// immediately.
		go func(from, to string) {
			_ = monitor.RefreshActive(from)
			_ = monitor.RefreshActive(to)
		}(from.Email, to.Email)

		// Update the manual-switch guard. A deliberate /switch sets the
		// guard; a rate-limit or threshold swap clears it (necessity wins).
		if p.trigger == history.TriggerManual {
			lastManualTarget = to.Email
			setManualSwitchState(to.Email)
		} else {
			lastManualTarget = ""
			setManualSwitchState("")
		}

		canResume := sessionID != "" && cfg.AutoResume && hadTurns
		if canResume {
			// Only resume if at least one turn completed — an empty/just-started
			// session has no transcript content and claude rejects --resume for it.
			// This applies to all trigger types including manual /switch.
			switch p.trigger {
			case history.TriggerRateLimit:
				fmt.Fprintf(w, "cux: rate limit on %s → swapped to %s, resuming…\n", from.Email, to.Email)
			case history.TriggerManual:
				fmt.Fprintf(w, "cux: %s → %s, resuming…\n", from.Email, to.Email)
			default:
				fmt.Fprintf(w, "cux: %s → %s (%s), resuming…\n", from.Email, to.Email, p.reason)
			}
			waitForTranscript(cwd, sessionID, transcriptWaitTimeout)
			currentArgv = append(relaunchFlags(argv), "--resume", sessionID)
			if p.resumeMessage != "" {
				// Write a one-shot flag so the UserPromptSubmit hook skips the
				// threshold check for this replayed prompt. Without this, if the
				// new account is also at/above the threshold the hook would block
				// the replayed prompt and trigger another switch — an infinite loop.
				_ = os.WriteFile(paths.ReplayFlagFile(pid), []byte("1"), 0o600)
				currentArgv = append(currentArgv, p.resumeMessage)
			} else if cfg.AutoMessage != "" {
				currentArgv = append(currentArgv, cfg.AutoMessage)
			}
			resumeRetryPending = true
		} else {
			// No session to resume — Claude will start fresh (welcome screen).
			// Print the switch result so the user knows which account is now active.
			fmt.Fprintf(w, "cux: switched to %s — now active\n", to.Email)
			currentArgv = argv
		}
	}
}

func utilizationOrZero(w *usage.Window) float64 {
	if w == nil {
		return 0
	}
	return w.Utilization
}

func writeWrapperPID(pid int) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(paths.ClaudePIDFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func cleanupWrapperPID(pid int) {
	b, err := os.ReadFile(paths.ClaudePIDFile())
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == strconv.Itoa(pid) {
		_ = os.Remove(paths.ClaudePIDFile())
	}
}

// setManualSwitchState persists the manual-switch target into state.json
// so that other running wrapper instances can also respect the user's choice.
// email="" clears the guard (called after rate-limit or threshold switches).
func setManualSwitchState(email string) {
	st, err := store.Load()
	if err != nil {
		return
	}
	st.ManualSwitchEmail = email
	if email != "" {
		st.ManualSwitchAt = time.Now().UTC()
	} else {
		st.ManualSwitchAt = time.Time{}
	}
	_ = st.Save()
}

// launch runs claude once, polling for signals until the child exits.
// Returns the child's exit code, the session_id we observed (if any),
// whether at least one turn completed (Stop signal fired), and a non-nil
// pending struct if the wrapper has decided to swap.
func launch(claudeBin string, argv []string, wrapperPID int, cfg *config.Config, manualTarget string, w io.Writer) (int, string, bool, *pending, error) {
	cmd := exec.Command(claudeBin, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		envWrapped+"=1",
		envWrapperPID+"="+strconv.Itoa(wrapperPID),
	)
	if err := cmd.Start(); err != nil {
		return 1, "", false, nil, fmt.Errorf("wrapper: start claude: %w", err)
	}

	var (
		mu        sync.Mutex
		sessionID string
		swap      *pending
	)
	var stopRequested atomic.Bool
	var hadTurns atomic.Bool // true once the first Stop signal fires

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				step(wrapperPID, cfg, manualTarget, &mu, &sessionID, &swap, &stopRequested, &hadTurns, cmd, w)
			}
		}
	}()

	waitErr := cmd.Wait()
	cancel()
	<-pollDone

	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return 1, sessionID, hadTurns.Load(), nil, waitErr
		}
	}

	// Read state under the mutex, then drop it before doing any
	// filesystem work. The poll goroutine has exited at this point so
	// there is no current contender, but keeping I/O outside the lock
	// is the right discipline if anyone adds another reader later.
	mu.Lock()
	finalSessionID := sessionID
	finalSwap := swap
	mu.Unlock()

	if finalSessionID == "" {
		// Fall back to a filesystem scan for older Claude Code versions
		// where the SessionStart hook may not have fired before exit.
		if cwd, err := os.Getwd(); err == nil {
			finalSessionID = bestEffortSessionID(cwd)
		}
	}
	return exitCode, finalSessionID, hadTurns.Load(), finalSwap, nil
}

// step is one tick of the poll loop. It consumes any signals present
// for this wrapper PID and updates state accordingly.
func step(
	wrapperPID int,
	cfg *config.Config,
	manualTarget string,
	mu *sync.Mutex,
	sessionID *string,
	swap **pending,
	stopRequested *atomic.Bool,
	hadTurns *atomic.Bool,
	cmd *exec.Cmd,
	w io.Writer,
) {
	// 1. Capture session-started if we haven't yet.
	if b, ok, _ := signals.Read(wrapperPID, signals.SessionStarted); ok {
		_ = signals.Consume(wrapperPID, signals.SessionStarted)
		if p, err := signals.DecodeSessionStarted(b); err == nil && p.SessionID != "" {
			mu.Lock()
			*sessionID = p.SessionID
			mu.Unlock()
		}
	}

	// 2. Rate limit ⇒ mark a swap pending and ask Claude to exit now.
	//    At a hard usage cap there may be no later Stop event, so
	//    waiting for one can strand the user on the exhausted account.
	//    We consume the signal even when AutoSwitchOnRateLimit is
	//    false, so a stale signal doesn't trigger a swap once the user
	//    re-enables it.
	if b, ok, _ := signals.Read(wrapperPID, signals.RateLimited); ok {
		_ = signals.Consume(wrapperPID, signals.RateLimited)
		if cfg.AutoSwitchOnRateLimit {
			hasSwap := false
			mu.Lock()
			if *swap == nil {
				msg := "rate-limit error from API"
				if p, err := signals.DecodeRateLimited(b); err == nil && p.Message != "" {
					msg = p.Message
				}
				*swap = &pending{trigger: history.TriggerRateLimit, reason: msg, fromUsage: snapshotActiveUsage()}
			}
			hasSwap = *swap != nil
			mu.Unlock()
			if hasSwap && stopRequested.CompareAndSwap(false, true) {
				go gracefulExit(cmd, w)
				return
			}
		} else {
			fmt.Fprintln(w, "cux: rate-limit hook fired but auto_switch_on_rate_limit is off; staying on current account")
		}
	}

	// 3. Manual /switch request from the slash command. Once the local
	//    slash command has run, there is no need to wait for another
	//    model turn before swapping.
	if b, ok, _ := signals.Read(wrapperPID, signals.SwitchRequested); ok {
		_ = signals.Consume(wrapperPID, signals.SwitchRequested)
		p, _ := signals.DecodeSwitchRequested(b)
		hasSwap := false
		mu.Lock()
		if *swap == nil {
			reason := "user requested via /switch"
			if p.ResumeMessage != "" {
				reason = "prompt intercepted before threshold swap"
			}
			*swap = &pending{
				trigger:        history.TriggerManual,
				reason:         reason,
				explicitTarget: p.Target,
				resumeMessage:  p.ResumeMessage,
				fromUsage:      snapshotActiveUsage(),
			}
		}
		hasSwap = *swap != nil
		mu.Unlock()
		if hasSwap && stopRequested.CompareAndSwap(false, true) {
			go gracefulExit(cmd, w)
			return
		}
	}

	// 4. Stop signal: a turn just ended cleanly.
	//
	// Order matters here. We refresh the active account's usage
	// synchronously *before* checking thresholds, so the threshold
	// decision uses the freshest data. The cost is one HTTP round-trip
	// (~150 ms) at turn-end, which is invisible against claude's own
	// turn latency. If we deferred the refresh to a goroutine the
	// threshold check would read stale data left from a prior Stop.
	if b, ok, _ := signals.Read(wrapperPID, signals.Stopped); ok {
		_ = signals.Consume(wrapperPID, signals.Stopped)
		hadTurns.Store(true)
		if p, err := signals.DecodeStopped(b); err == nil && p.SessionID != "" {
			mu.Lock()
			*sessionID = p.SessionID
			mu.Unlock()
		}
		if email, err := switcher.CurrentLiveEmail(); err == nil {
			_ = monitor.RefreshActive(email)
		}
		mu.Lock()
		if *swap == nil && cfg.AutoSwitchOnThreshold {
			if p := evaluateThresholdSwap(cfg, manualTarget); p != nil {
				*swap = p
			}
		}
		hasSwap := *swap != nil
		mu.Unlock()
		if hasSwap && stopRequested.CompareAndSwap(false, true) {
			go gracefulExit(cmd, w)
			return
		}
	}
}

// snapshotActiveUsage returns whatever the cache currently has for the
// live account so a swap entry can record where we were when we
// decided to leave. Best-effort; if the cache is empty the entry just
// shows zero usage which the history printer renders as "—".
func snapshotActiveUsage() usage.AccountUsage {
	email, _ := switcher.CurrentLiveEmail()
	cacheKey, err := switcher.CurrentLiveCacheKey()
	if err != nil {
		return usage.AccountUsage{}
	}
	cache, err := usage.LoadCache()
	if err != nil {
		return usage.AccountUsage{}
	}
	u, _ := cachedUsage(cache, cacheKey, email)
	return u
}

// evaluateThresholdSwap returns a pending if the active account's
// cached usage has crossed the configured thresholds. Returns nil
// when no swap is warranted, when the cache has no entry yet, or
// when no other account has spare capacity per strategy.
//
// manualTarget suppresses auto-switch when the live account was
// deliberately placed there by a /switch command (in this wrapper or
// any other — checked via state.ManualSwitchEmail). Necessity (rate
// limit) overrides this guard and clears it.
func evaluateThresholdSwap(cfg *config.Config, manualTarget string) *pending {
	email, err := switcher.CurrentLiveEmail()
	if err != nil {
		return nil
	}

	// Read usage first — at hard limit (100%) necessity overrides any
	// manual-switch guard, so the guard must be evaluated after we know
	// whether this is a soft-threshold or hard-limit case.
	cache, err := usage.LoadCache()
	if err != nil || cache == nil {
		return nil
	}
	cacheKey, err := switcher.CurrentLiveCacheKey()
	if err != nil {
		return nil
	}
	u, ok := cachedUsage(cache, cacheKey, email)
	if !ok {
		return nil
	}
	over, reason := usage.IsOverThreshold(u, cfg.Thresholds)
	if !over {
		return nil
	}

	// Hard-limit check: 5h or 7d at exactly 100% → bypass both guards.
	// At soft-threshold triggers we still respect manual choices.
	atHardLimit := (u.FiveHour != nil && u.FiveHour.Utilization >= 100) ||
		(u.SevenDay != nil && u.SevenDay.Utilization >= 100)
	if !atHardLimit {
		// Layer 1: in-wrapper guard.
		if manualTarget != "" && email == manualTarget {
			return nil
		}
		// Layer 2: cross-wrapper guard — another session manually placed
		// this account here; respect their choice.
		if st, err := store.Load(); err == nil {
			if st.ManualSwitchEmail == email && !st.ManualSwitchAt.IsZero() {
				return nil
			}
		}
	}

	state, err := store.Load()
	if err != nil {
		return nil
	}
	candidates := make([]strategy.Candidate, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		candidates = append(candidates, strategy.Candidate{Email: a.Email, CacheKey: a.CacheKey()})
	}
	current := strategy.Candidate{Email: email, CacheKey: cacheKey}
	if _, ok := cache[cacheKey]; !ok {
		if _, emailOK := cache[email]; emailOK {
			current.CacheKey = email
		}
	}
	pick, ok := strategy.PickNext(cfg.ResolvedStrategy(), cfg.Strategy.Order, candidates,
		current, cache, cfg.Thresholds)
	if !ok {
		// Nothing to swap to — let claude continue on the maxed-out
		// account; the rate-limit hook will catch the actual cap.
		return nil
	}
	return &pending{
		trigger:        history.TriggerThreshold,
		reason:         reason,
		explicitTarget: pick.Email,
		fromUsage:      u,
	}
}

func shouldPreflightHardLimit(argv []string) bool {
	for _, arg := range argv {
		if arg == "--resume" || arg == "-r" {
			return true
		}
	}
	return false
}

func evaluatePrelaunchHardLimitSwap(cfg *config.Config) (*pending, string) {
	p := evaluateThresholdSwap(cfg, "")
	if p != nil && isHardLimitUsage(p.fromUsage) {
		p.trigger = history.TriggerRateLimit
		p.reason = p.reason + " before launch"
		return p, ""
	}

	if isActiveHardLimited() {
		if msg := renderAllAccountsExhaustedMessage(cfg); msg != "" {
			return nil, msg
		}
	}
	return nil, ""
}

func isHardLimitUsage(u usage.AccountUsage) bool {
	return (u.FiveHour != nil && u.FiveHour.Utilization >= 100) ||
		(u.SevenDay != nil && u.SevenDay.Utilization >= 100)
}

func appendPrelaunchHistory(from, to store.Account, p *pending) {
	cwd, _ := os.Getwd()
	toUsageCache, _ := usage.LoadCache()
	toUsage, _ := cachedUsage(toUsageCache, to.CacheKey(), to.Email)
	_ = history.Append(history.Entry{
		From:        from.Email,
		To:          to.Email,
		Trigger:     p.trigger,
		Reason:      p.reason,
		CWD:         cwd,
		FromUsage5h: utilizationOrZero(p.fromUsage.FiveHour),
		FromUsage7d: utilizationOrZero(p.fromUsage.SevenDay),
		ToUsage5h:   utilizationOrZero(toUsage.FiveHour),
		ToUsage7d:   utilizationOrZero(toUsage.SevenDay),
	})
}

func isActiveHardLimited() bool {
	email, err := switcher.CurrentLiveEmail()
	if err != nil {
		return false
	}
	cacheKey, err := switcher.CurrentLiveCacheKey()
	if err != nil {
		cacheKey = email
	}
	cache, err := usage.LoadCache()
	if err != nil {
		return false
	}
	u, ok := cachedUsage(cache, cacheKey, email)
	return ok && isHardLimitUsage(u)
}

func cachedUsage(cache usage.Cache, cacheKey, email string) (usage.AccountUsage, bool) {
	if cache == nil {
		return usage.AccountUsage{}, false
	}
	if cacheKey != "" {
		if u, ok := cache[cacheKey]; ok {
			return u, true
		}
	}
	if email != "" {
		if u, ok := cache[email]; ok {
			return u, true
		}
	}
	return usage.AccountUsage{}, false
}

func renderAllAccountsExhaustedMessage(cfg *config.Config) string {
	state, err := store.Load()
	if err != nil || len(state.Accounts) == 0 {
		return ""
	}
	cache, err := usage.LoadCache()
	if err != nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("cux: all managed accounts are exhausted\n")
	b.WriteString("cux: no Claude usage is available right now.\n\n")
	b.WriteString("Accounts:\n")

	var nextEmail string
	var nextReset *time.Time
	for _, slot := range state.SortedSlots() {
		acct := state.Accounts[slot]
		u, _ := cachedUsage(cache, acct.CacheKey(), acct.Email)
		reset := availabilityReset(u)
		b.WriteString(fmt.Sprintf("  [%02d] %s  5h:%s  7d:%s", slot, acct.Email, pct(u.FiveHour), pct(u.SevenDay)))
		if reset != nil {
			b.WriteString("  available in " + shortDuration(time.Until(*reset)))
			if nextReset == nil || reset.Before(*nextReset) {
				t := *reset
				nextReset = &t
				nextEmail = acct.Email
			}
		} else if accountHasSwitchCapacity(cache, acct.CacheKey(), cfg) {
			b.WriteString("  available now")
		} else {
			b.WriteString("  reset unknown")
		}
		b.WriteString("\n")
	}

	if nextReset != nil {
		b.WriteString(fmt.Sprintf("\nNext available account: %s in %s\n", nextEmail, shortDuration(time.Until(*nextReset))))
	} else {
		b.WriteString("\nNext available account: unknown; run `cux usage refresh` later.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func availabilityReset(u usage.AccountUsage) *time.Time {
	if u.SevenDay != nil && u.SevenDay.Utilization >= 100 {
		return u.SevenDay.ResetsAt
	}
	if u.FiveHour != nil && u.FiveHour.Utilization >= 100 {
		return u.FiveHour.ResetsAt
	}
	return nil
}

func pct(w *usage.Window) string {
	if w == nil {
		return "--"
	}
	return fmt.Sprintf("%.0f%%", w.Utilization)
}

func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// gracefulExit asks the child to shut down. We send the OS-appropriate
// "interrupt" signal first (matches what Ctrl-C would do), wait briefly
// for claude to flush and exit on its own, then escalate to a hard
// terminate if it's still alive.
//
// Threshold swaps wait for a Stop signal before calling this, so the
// transcript is usually flushed. Rate-limit and manual /switch paths
// may interrupt mid-turn; Run waits for the transcript file to settle
// before relaunching with --resume.
func gracefulExit(cmd *exec.Cmd, w io.Writer) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)

	deadline := time.NewTimer(gracefulExitWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline.C:
			fmt.Fprintln(w, "cux: claude did not exit cleanly, terminating…")
			_ = cmd.Process.Kill()
			return
		case <-tick.C:
			if cmd.ProcessState != nil {
				return
			}
		}
	}
}

// resolveTarget converts the wrapper's pending decision into the
// account identifier the switcher package accepts.
//
// Order of preference:
//  1. The explicit target (/switch typed an email or slot).
//  2. strategy.PickNext under the configured kind/order — for
//     manual/rate-limit/threshold triggers without an explicit target.
//  3. Capacity-aware rotation as a last-resort fallback when strategy
//     returns no candidate, e.g. Manual mode or sparse fresh-install
//     usage data.
func resolveTarget(explicit string, trigger history.Trigger, cfg *config.Config) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	state, err := store.Load()
	if err != nil {
		return "", err
	}
	if len(state.Accounts) < 2 {
		return "", errors.New("only one account is managed; nothing to rotate to")
	}
	current, _ := switcher.CurrentLiveEmail()
	currentCacheKey, _ := switcher.CurrentLiveCacheKey()
	if currentCacheKey == "" {
		currentCacheKey = current
	}

	// Try strategy first.
	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	candidates := make([]strategy.Candidate, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		candidates = append(candidates, strategy.Candidate{Email: a.Email, CacheKey: a.CacheKey()})
	}
	// In manual mode strategy.PickNext returns ok=false, but a manual
	// trigger with no explicit target still means "rotate" — fall
	// through to bare rotation in that case.
	kind := cfg.ResolvedStrategy()
	if trigger == history.TriggerManual && kind == strategy.KindManual {
		return rotateFallback(state, cache, cfg)
	}
	if pick, ok := strategy.PickNext(kind, cfg.Strategy.Order, candidates,
		strategy.Candidate{Email: current, CacheKey: currentCacheKey}, cache, cfg.Thresholds); ok {
		return pick.Email, nil
	}
	return rotateFallback(state, cache, cfg)
}

// rotateFallback walks store's rotation order, but still refuses accounts
// that are known to have no capacity. Missing usage is treated as usable so
// fresh installs can rotate before the first refresh completes.
func rotateFallback(state *store.State, cache usage.Cache, cfg *config.Config) (string, error) {
	for _, slot := range rotationSlots(state) {
		acct, ok := state.Accounts[slot]
		if !ok || slot == state.ActiveSlot {
			continue
		}
		if accountHasSwitchCapacity(cache, acct.CacheKey(), cfg) {
			return strconv.Itoa(slot), nil
		}
	}
	return "", errors.New("no usable accounts available; all managed accounts are exhausted or need login")
}

func rotationSlots(state *store.State) []int {
	slots := state.SortedSlots()
	if next := state.NextInRotation(state.ActiveSlot); next != 0 {
		for i, slot := range slots {
			if slot == next {
				return append(slots[i:], slots[:i]...)
			}
		}
	}
	return slots
}

func accountHasSwitchCapacity(cache usage.Cache, cacheKey string, cfg *config.Config) bool {
	u, ok := cache[cacheKey]
	if !ok {
		return true
	}
	if u.TokenExpired {
		return false
	}
	if u.SevenDay != nil && u.SevenDay.Utilization >= 100 {
		return false
	}
	cap5 := cfg.Thresholds.FiveHour
	if cap5 == 0 || cap5 == 100 {
		cap5 = 90
	}
	return u.FiveHour == nil || u.FiveHour.Utilization < float64(cap5)
}

// sessionFlags are claude's session-selection arguments. The wrapper
// substitutes its own `--resume <id>` on relaunch, so whatever the user
// passed to pick a session must not survive alongside it.
var sessionFlags = map[string]bool{
	"--resume": true, "-r": true,
	"--continue": true, "-c": true,
	"--session-id":   true,
	"--fork-session": true,
}

// sessionValueFlags are the session flags that may consume the next
// token as their value (`--resume <id>`, `--session-id <uuid>`).
var sessionValueFlags = map[string]bool{
	"--resume": true, "-r": true,
	"--session-id": true,
}

// claudeBoolFlags are claude flags known to never take a value, so a bare
// token right after one of them is a positional prompt rather than the
// flag's value. The list doesn't need to be exhaustive: an unknown
// boolean flag just means a trailing prompt is kept as its "value" and
// replayed into the resumed session — mildly redundant, never fatal.
var claudeBoolFlags = map[string]bool{
	"--dangerously-skip-permissions": true,
	"--print":                        true,
	"-p":                             true,
	"--verbose":                      true,
	"--ide":                          true,
	"--strict-mcp-config":            true,
}

// relaunchFlags returns the user's original claude arguments minus
// session selection and minus positional prompts, so a post-swap
// relaunch keeps flags like --dangerously-skip-permissions or --model
// while the wrapper controls which session is resumed. Positional
// prompts are dropped because a resumable session (hadTurns) already
// has them in its transcript; replaying one would duplicate the turn.
func relaunchFlags(argv []string) []string {
	out := []string{}
	expectValue := false
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			break // everything after the separator is positional
		}
		isFlag := len(tok) > 1 && tok[0] == '-'
		if !isFlag {
			if expectValue {
				out = append(out, tok)
				expectValue = false
			}
			continue
		}
		expectValue = false
		name, _, hasEq := strings.Cut(tok, "=")
		if sessionFlags[name] {
			if !hasEq && sessionValueFlags[name] &&
				i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++ // skip the flag's session-id value too
			}
			continue
		}
		out = append(out, tok)
		if !hasEq && !claudeBoolFlags[name] {
			expectValue = true
		}
	}
	return out
}

func isResumeArgv(argv []string) bool {
	for _, arg := range argv {
		if arg == "--resume" || arg == "-r" {
			return true
		}
	}
	return false
}

func resumeSessionID(argv []string) string {
	for i, arg := range argv {
		if (arg == "--resume" || arg == "-r") && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func transcriptPath(cwd, sessionID string) string {
	return filepath.Join(paths.ProjectTranscriptDir(cwd), sessionID+".jsonl")
}

// waitForTranscript blocks until the session transcript exists, is
// non-empty, and its size has been stable for transcriptStableWindow,
// or until timeout. Returns false when the file never becomes ready.
func waitForTranscript(cwd, sessionID string, timeout time.Duration) bool {
	if cwd == "" || sessionID == "" {
		return false
	}
	path := transcriptPath(cwd, sessionID)
	deadline := time.Now().Add(timeout)
	var (
		lastSize    int64 = -1
		stableSince time.Time
	)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			lastSize = -1
			stableSince = time.Time{}
			time.Sleep(transcriptPollInterval)
			continue
		}
		size := fi.Size()
		if size != lastSize {
			lastSize = size
			stableSince = time.Now()
		} else if !stableSince.IsZero() && time.Since(stableSince) >= transcriptStableWindow {
			return true
		}
		time.Sleep(transcriptPollInterval)
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// bestEffortSessionID is the v0.1 fallback for capturing session id —
// scan the project's transcript dir for the newest .jsonl. Used only
// when the SessionStart hook didn't fire (unwrapped session, or
// Claude Code version that omits the hook).
func bestEffortSessionID(cwd string) string {
	dir := paths.ProjectTranscriptDir(cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var (
		newest    string
		newestMod int64
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 7 || name[len(name)-6:] != ".jsonl" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if mod := fi.ModTime().UnixNano(); mod > newestMod {
			newestMod = mod
			newest = name
		}
	}
	if newest == "" {
		return ""
	}
	return newest[:len(newest)-6]
}
