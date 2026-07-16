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
	"bytes"
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
	"github.com/inulute/cux/internal/ptyhost"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/strategy"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
	"golang.org/x/term"
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

// crlfWriter translates \n to \r\n so wrapper narration renders
// correctly while the terminal is in raw mode for the PTY host.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	out := bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

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
	retryOnly      bool               // relaunch the same account; no swap
	fromUsage      usage.AccountUsage // best-effort snapshot
	fromKey        string             // cache key of the seat live when the swap was decided; lets a rate-limit swap tell "another session already moved us" from "still on the exhausted seat"
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
	// Heartbeat: self-report what this wrapper is doing so that
	// `cux sessions` (and remote surfaces reading the same registry)
	// can show every concurrent session at a glance.
	registry.UpdateSelf(func(e *registry.Entry) { e.State = registry.StateRunning })
	defer registry.RemoveSelf()

	// Attachable sessions: when stdin is a real terminal, run claude on
	// a wrapper-owned PTY and serve mirrors on a Unix socket. The PTY
	// outlives individual claude launches, so attached viewers ride
	// straight through account swaps. Non-TTY stdin (pipes, CI) keeps
	// the plain inherit-stdio path untouched.
	var host *ptyhost.Host
	if cfg.Attach && term.IsTerminal(int(os.Stdin.Fd())) {
		_ = os.MkdirAll(paths.AttachDir(), 0o700)
		// Crashed wrappers leave their sockets behind; sweep the dead
		// ones so attach surfaces never probe endpoints that can't answer.
		registry.ReapStaleAttachSockets()
		if h, err := ptyhost.New(paths.AttachSock(pid), cfg.AttachInput); err == nil {
			host = h
			defer host.Close()
			if old, rawErr := term.MakeRaw(int(os.Stdin.Fd())); rawErr == nil {
				defer func() { _ = term.Restore(int(os.Stdin.Fd()), old) }()
			}
			go host.Pump()
			// Wrapper narration ("cux: …" lines) goes to the real
			// terminal (raw mode needs CRLF) and to attached viewers.
			w = io.MultiWriter(crlfWriter{os.Stdout}, host.BroadcastWriter())
			registry.UpdateSelf(func(e *registry.Entry) { e.Attachable = true })
		} else {
			fmt.Fprintf(w, "cux: attach disabled: %v\n", err)
		}
	}

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
	// apiRetries counts consecutive same-account relaunches after API
	// failures; it scales the fibonacci backoff and resets whenever a
	// turn completes or claude exits for any other reason. There is no
	// upper bound by design: an unattended session should outlast even
	// a multi-hour outage, and a human can always Ctrl+C.
	var apiRetries int

	for {
		if seat, err := switcher.CurrentLiveEmail(); err == nil {
			registry.UpdateSelf(func(e *registry.Entry) {
				e.State = registry.StateRunning
				e.Seat = seat
				e.Detail = ""
			})
		}
		exitCode, sessionID, hadTurns, p, err := launch(claudeBin, currentArgv, pid, &cfg, lastManualTarget, host, w)
		if err != nil {
			return exitCode, err
		}
		if sessionID != "" {
			registry.UpdateSelf(func(e *registry.Entry) { e.SessionID = sessionID })
		}

		if p != nil && p.retryOnly {
			if hadTurns {
				apiRetries = 0
			}
			// A hard usage limit can reach us dressed as a generic API
			// failure: the classifier only sees error text, and Claude
			// Code's wording for the cap shifts between builds. Backoff is
			// the wrong tool for exhaustion — the account has no capacity
			// to retry into, and when the whole pool is out the reset
			// clocks are all known. Check fresh usage and, if the live
			// account is actually out, promote to the rate-limit path
			// (swap if anything is usable, else wait-for-reset sleeps
			// until the earliest known reset) instead of fixed-interval
			// relaunches that spin against a closed window until a human
			// returns.
			_, _ = monitor.RefreshAll()
			if isActiveHardLimited() {
				lk, _ := switcher.CurrentLiveCacheKey()
				fmt.Fprintf(w, "cux: that API failure is a usage limit on the live account — switching instead of retrying\n")
				p = &pending{trigger: history.TriggerRateLimit, reason: p.reason, fromUsage: snapshotActiveUsage(), fromKey: lk}
			}
		}
		if p != nil && p.retryOnly {
			delay := fibonacciDelay(apiRetries)
			apiRetries++
			fmt.Fprintf(w, "cux: API failure after claude's own retries (%s) — auto-continuing in %s (attempt %d)…\n",
				snippet(p.reason), shortDuration(delay), apiRetries)
			registry.UpdateSelf(func(e *registry.Entry) {
				e.State = registry.StateRetrying
				e.Detail = fmt.Sprintf("attempt %d, next try in %s", apiRetries, shortDuration(delay))
			})
			time.Sleep(delay)
			// A StopFailure implies a session existed and a turn was
			// attempted, so treat a hook-reported session as resumable
			// even without a completed turn.
			if resumeSID, ok := resumableSession(sessionID, currentArgv, cfg.AutoResume, sessionID != ""); ok {
				cwd, _ := os.Getwd()
				waitForTranscript(cwd, resumeSID, transcriptWaitTimeout)
				// Preserve the user's original flags (e.g.
				// --dangerously-skip-permissions, --model) on the retry
				// relaunch, same as the swap path below (#11).
				currentArgv = append(relaunchFlags(argv), "--resume", resumeSID)
				if cfg.AutoMessage != "" {
					currentArgv = append(currentArgv, cfg.AutoMessage)
				}
				resumeRetryPending = true
			} else {
				currentArgv = argv
			}
			continue
		}
		apiRetries = 0

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

		// Concurrent sessions all see the same rate limit at once: the
		// first wrapper through the lock swaps, and without this check
		// every other one would land on the freshly-switched, healthy
		// account and still swap again — one pointless hop per session,
		// burning through the whole pool. Refresh first so the verdict
		// uses current data, then skip the swap when the live account
		// already has room. Manual switches are never skipped.
		swapped := true
		var from, to store.Account
		if p.explicitTarget == "" &&
			(p.trigger == history.TriggerRateLimit || p.trigger == history.TriggerThreshold) {
			_, _ = monitor.RefreshAll()
			if acct, ok := liveAccountWithCapacity(&cfg); ok && skipSwapOnCapacity(p.trigger, acct.CacheKey(), p.fromKey) {
				fmt.Fprintf(w, "cux: %s already has capacity (another session may have swapped) — resuming without switching\n", acct.Email)
				from, to = acct, acct
				swapped = false
			}
		}

		cwd, _ := os.Getwd()
		if swapped {
			target, err := resolveTarget(p.explicitTarget, p.trigger, &cfg)
			if err != nil && cfg.WaitForReset && p.explicitTarget == "" &&
				(p.trigger == history.TriggerRateLimit || p.trigger == history.TriggerThreshold) {
				target, err = waitForReset(p.trigger, &cfg, w)
			}
			if err != nil {
				fmt.Fprintf(w, "cux: %v — staying on current account\n", err)
				return exitCode, nil
			}

			registry.UpdateSelf(func(e *registry.Entry) { e.State = registry.StateSwapping })
			var swapErr error
			from, to, swapErr = switcher.SwitchTo(target)
			if swapErr != nil {
				fmt.Fprintf(w, "cux: switch failed: %v\n", swapErr)
				return 1, swapErr
			}

			// Append swap to the history log. Best-effort — a failure
			// here doesn't unwind the swap.
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
		}

		// Update the manual-switch guard. A deliberate /switch sets the
		// guard; a rate-limit or threshold swap clears it (necessity wins).
		if p.trigger == history.TriggerManual {
			lastManualTarget = to.Email
			setManualSwitchState(to.Email)
		} else {
			lastManualTarget = ""
			setManualSwitchState("")
		}

		resumeSID, canResume := resumableSession(sessionID, currentArgv, cfg.AutoResume, hadTurns)
		if canResume {
			// Only resume if at least one turn completed — an empty/just-started
			// session has no transcript content and claude rejects --resume for it.
			// This applies to all trigger types including manual /switch.
			switch {
			case !swapped:
				fmt.Fprintf(w, "cux: resuming on %s…\n", to.Email)
			case p.trigger == history.TriggerRateLimit:
				fmt.Fprintf(w, "cux: rate limit on %s → swapped to %s, resuming…\n", from.Email, to.Email)
			case p.trigger == history.TriggerManual:
				fmt.Fprintf(w, "cux: %s → %s, resuming…\n", from.Email, to.Email)
			default:
				fmt.Fprintf(w, "cux: %s → %s (%s), resuming…\n", from.Email, to.Email, p.reason)
			}
			waitForTranscript(cwd, resumeSID, transcriptWaitTimeout)
			currentArgv = append(relaunchFlags(argv), "--resume", resumeSID)
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
			if swapped {
				fmt.Fprintf(w, "cux: switched to %s — now active\n", to.Email)
			} else {
				fmt.Fprintf(w, "cux: continuing on %s\n", to.Email)
			}
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
func launch(claudeBin string, argv []string, wrapperPID int, cfg *config.Config, manualTarget string, host *ptyhost.Host, w io.Writer) (int, string, bool, *pending, error) {
	env := append(os.Environ(),
		envWrapped+"=1",
		envWrapperPID+"="+strconv.Itoa(wrapperPID),
	)
	// startChild is platform-specific: Unix runs claude on a PTY slave
	// (see start_other.go), Windows on a ConPTY (start_windows.go). The
	// swap/poll/wait logic below drives it through the `child` interface.
	ch, err := startChild(claudeBin, argv, env, host)
	if err != nil {
		return 1, "", false, nil, fmt.Errorf("wrapper: start claude: %w", err)
	}
	// Tell the host which process to nudge with SIGWINCH on resize: the
	// child runs without a controlling terminal, so a PTY size change does
	// not raise SIGWINCH on its own. Refreshed on every (re)launch.
	if host != nil {
		host.SetChildPID(ch.Pid())
		defer host.SetChildPID(0)
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
				step(wrapperPID, cfg, manualTarget, &mu, &sessionID, &swap, &stopRequested, &hadTurns, ch, w)
			}
		}
	}()

	waitErr := ch.Wait()
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
	ch child,
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
				lk, _ := switcher.CurrentLiveCacheKey()
				*swap = &pending{trigger: history.TriggerRateLimit, reason: msg, fromUsage: snapshotActiveUsage(), fromKey: lk}
			}
			hasSwap = *swap != nil
			mu.Unlock()
			if hasSwap && stopRequested.CompareAndSwap(false, true) {
				go gracefulExit(ch, w)
				return
			}
		} else {
			fmt.Fprintln(w, "cux: rate-limit hook fired but auto_switch_on_rate_limit is off; staying on current account")
		}
	}

	// 2b. Non-rate-limit API failure ⇒ relaunch the same account after
	//     a backoff. The turn is already dead (StopFailure fires only
	//     after Claude Code exhausted its own retries), so like the
	//     rate-limit case there may be no later Stop event to wait for.
	if b, ok, _ := signals.Read(wrapperPID, signals.TurnFailed); ok {
		_ = signals.Consume(wrapperPID, signals.TurnFailed)
		if cfg.RetryOnAPIError {
			hasSwap := false
			mu.Lock()
			if *swap == nil {
				msg := "API error after retries"
				if p, err := signals.DecodeTurnFailed(b); err == nil && p.Message != "" {
					msg = p.Message
				}
				*swap = &pending{retryOnly: true, reason: msg}
			}
			hasSwap = *swap != nil
			mu.Unlock()
			if hasSwap && stopRequested.CompareAndSwap(false, true) {
				go gracefulExit(ch, w)
				return
			}
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
			go gracefulExit(ch, w)
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
			go gracefulExit(ch, w)
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
	pool := state.PoolForCwd()
	candidates := make([]strategy.Candidate, 0, len(pool))
	for _, a := range pool {
		candidates = append(candidates, strategy.Candidate{Email: a.Email, Slot: a.Slot, CacheKey: a.CacheKey()})
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
		explicitTarget: pick.Identifier(),
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

// waitForReset blocks until the earliest moment a managed account
// becomes usable again, refreshes usage, and retries target resolution.
// Reached only when every account is exhausted — the alternative was
// giving up and stranding an unattended session until a human returns.
// A few attempts guard against clock skew between the local machine
// and the API's reset stamps; each retry waits for the next reset, so
// even the fallback path never spins.
const (
	waitForResetAttempts = 4
	// waitPollInterval is how often a sleeping waitForReset re-reads
	// the shared cache for capacity another session may have created.
	waitPollInterval = time.Minute
	// resetSlack pads each sleep so we wake after the API-side window
	// has actually rolled over, not in the same second it should.
	resetSlack = 90 * time.Second
	// resetBuffer is extra margin on top of resetSlack so a resume lands
	// a bit after the reset rather than exactly on it.
	resetBuffer = time.Minute
)

func waitForReset(trigger history.Trigger, cfg *config.Config, w io.Writer) (string, error) {
	for attempt := 0; attempt < waitForResetAttempts; attempt++ {
		// Check first, then time. Refresh from the API so the verdict and
		// the reset clock use current data — never a stale cache whose
		// resets_at may already be in the past (which used to yield a
		// bogus ~2-minute countdown and the wrong account).
		_, _ = monitor.RefreshAll()
		if target, err := resolveTarget("", trigger, cfg); err == nil {
			return target, nil // something is usable now — resume, no timer
		}

		state, err := store.Load()
		if err != nil {
			return "", err
		}
		cache, _ := usage.LoadCache()
		readyAt, email, ok := nextAvailability(state.PoolForCwd(), cache, cfg.Thresholds, time.Now())
		if !ok {
			return "", errors.New("all accounts exhausted and no reset time is known")
		}
		// resetSlack pads past the reset; add another minute of margin so
		// we resume a bit after the window rolls over rather than the
		// instant it should, avoiding an immediate re-hit on clock skew or
		// server-side rounding. Round(0) strips the monotonic clock reading
		// so the countdown is measured against the wall clock — it keeps
		// counting across a laptop sleep instead of freezing (Go timers use
		// the monotonic clock, which pauses while the system is asleep).
		d := time.Until(readyAt) + resetSlack + resetBuffer
		if d < resetSlack+resetBuffer {
			d = resetSlack + resetBuffer
		}
		deadline := time.Now().Add(d).Round(0)
		resumeClock := deadline.Format("3:04 PM")
		fmt.Fprintf(w, "cux: all accounts exhausted. %s reaches its reset first — resuming at %s (in %s).\n",
			email, resumeClock, countdownRemaining(time.Until(deadline)))
		registry.UpdateSelf(func(e *registry.Entry) {
			e.State = registry.StateWaitingReset
			e.Detail = fmt.Sprintf("resuming at %s (%s left)", resumeClock, countdownRemaining(time.Until(deadline)))
		})
		// Count down against the wall-clock deadline (re-read every tick,
		// so a system sleep can't freeze it). Tick every second in the last
		// ten minutes so the seconds visibly count down, otherwise once a
		// minute. Refresh + probe at most once per poll interval (wall-clock
		// gated, so it also fires promptly after a sleep) so we resume the
		// moment an account resets without hammering the API each tick.
		lastRefresh := time.Now().Round(0)
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			fmt.Fprintf(w, "\r\033[Kcux: resuming at %s · %s remaining…", resumeClock, countdownRemaining(remaining))
			tick := waitPollInterval
			if remaining < 10*time.Minute {
				tick = time.Second
			}
			time.Sleep(min(tick, remaining))
			if time.Now().Sub(lastRefresh) >= waitPollInterval {
				lastRefresh = time.Now().Round(0)
				_, _ = monitor.RefreshAll()
				if target, err := resolveTarget("", trigger, cfg); err == nil {
					fmt.Fprintf(w, "\ncux: %s has capacity — resuming\n", target)
					return target, nil
				}
			}
		}
		fmt.Fprintln(w) // close the in-place countdown line before relaunch output
		// Deadline reached — loop back to the check-first refresh above.
	}
	return "", errors.New("accounts still exhausted after waiting for resets")
}

// liveAccountWithCapacity returns the managed account currently
// holding the live credentials, when it still has switch capacity
// under the configured thresholds. ok is false when the live account
// is unmanaged, has no usage data yet, or is out of room — callers
// then proceed with a normal swap.
// skipSwapOnCapacity decides whether a pending swap may be skipped
// because the live account already reports capacity. A rate-limit signal
// is ground truth about the seat that triggered it: the API just refused
// it, so its utilisation numbers (a session cap, a burst cap, or a
// per-model Opus/Sonnet cap need not move the 5h/7d figures) cannot be
// trusted to say "usable". Skipping is therefore only safe on a
// rate-limit when the live seat is a DIFFERENT one than the one that hit
// the limit — i.e. another concurrent session already swapped us onto a
// healthy seat (#21). If the live seat is still the rate-limited one (or
// we couldn't identify it), we must swap away or wait — never resume in
// place, which loops forever on the exhausted account. Threshold swaps
// are proactive (no hard limit was hit), so capacity there is real.
func skipSwapOnCapacity(trigger history.Trigger, liveKey, rateLimitedKey string) bool {
	if trigger == history.TriggerRateLimit {
		return rateLimitedKey != "" && liveKey != "" && liveKey != rateLimitedKey
	}
	return true
}

func liveAccountWithCapacity(cfg *config.Config) (store.Account, bool) {
	email, err := switcher.CurrentLiveEmail()
	if err != nil || email == "" {
		return store.Account{}, false
	}
	state, err := store.Load()
	if err != nil {
		return store.Account{}, false
	}
	// Find the seat holding the live credentials by cache key first —
	// emails are not unique (the same address can hold a personal
	// subscription and one seat per org); email is the legacy fallback.
	liveKey, _ := switcher.CurrentLiveCacheKey()
	var acct store.Account
	found := false
	if liveKey != "" && liveKey != email {
		for _, a := range state.Accounts {
			if a.CacheKey() == liveKey {
				acct, found = a, true
				break
			}
		}
	}
	if !found {
		slot := state.FindByEmail(email)
		if slot == 0 {
			return store.Account{}, false
		}
		acct = state.Accounts[slot]
	}
	cache, _ := usage.LoadCache()
	u, ok := cachedUsage(cache, acct.CacheKey(), acct.Email)
	if !ok || u.TokenExpired {
		return store.Account{}, false
	}
	// accountHasSwitchCapacity looks the entry up by one key only;
	// hand it whichever key actually holds the data.
	key := acct.CacheKey()
	if _, exists := cache[key]; !exists {
		key = acct.Email
	}
	if !accountHasSwitchCapacity(cache, key, cfg) {
		return store.Account{}, false
	}
	return acct, true
}

// nextAvailability returns the earliest instant any account becomes
// usable again and which account that is. An account's ready time is
// the latest reset among its over-threshold windows — a 5h reset does
// not help while the 7d window is still capped. Accounts whose binding
// window carries no reset stamp are skipped: there is nothing to wait
// for. ok is false when no account has a known ready time.
func nextAvailability(
	accounts map[int]store.Account,
	cache usage.Cache,
	thresholds usage.Thresholds,
	now time.Time,
) (time.Time, string, bool) {
	var best time.Time
	var bestEmail string
	for _, acct := range accounts {
		u, found := cachedUsage(cache, acct.CacheKey(), acct.Email)
		if !found || u.TokenExpired {
			continue
		}
		readyAt := now
		unknown := false
		for _, wc := range []struct {
			win *usage.Window
			cap int
		}{
			{u.FiveHour, thresholds.FiveHour},
			{u.SevenDay, thresholds.SevenDay},
		} {
			if wc.win == nil || wc.win.Utilization < float64(wc.cap) {
				continue
			}
			if wc.win.ResetsAt == nil {
				unknown = true
				break
			}
			if wc.win.ResetsAt.After(readyAt) {
				readyAt = *wc.win.ResetsAt
			}
		}
		if unknown {
			continue
		}
		if bestEmail == "" || readyAt.Before(best) {
			best = readyAt
			bestEmail = acct.Email
		}
	}
	return best, bestEmail, bestEmail != ""
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

// countdownRemaining formats the wait-for-reset countdown: minute
// granularity normally, but seconds once under ten minutes so the final
// stretch visibly ticks down (e.g. "9m 53s", "45s").
func countdownRemaining(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 10*time.Minute {
		return shortDuration(d)
	}
	d = d.Round(time.Second)
	m := int(d / time.Minute)
	s := int(d % time.Minute / time.Second)
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
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
func gracefulExit(ch child, w io.Writer) {
	pid := ch.Pid()
	if pid == 0 {
		return
	}
	// Snapshot the descendant tree before signalling: claudeBin may be
	// a wrapper script chaining other tools (cux → headroom → claude,
	// issue #3). The SIGINT below reaches only the direct child, and a
	// dying shell does not forward it — grandchildren would survive,
	// stay attached to the terminal, and fight the relaunched claude
	// for stdin.
	strays := descendantPIDs(pid)
	_ = ch.Signal(os.Interrupt)

	deadline := time.NewTimer(gracefulExitWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline.C:
			fmt.Fprintln(w, "cux: claude did not exit cleanly, terminating…")
			_ = ch.Kill()
			reapStrays(strays, w)
			return
		case <-tick.C:
			if ch.Exited() {
				reapStrays(strays, w)
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
	// Rotation draws from the project pool for this directory (the full
	// account list when no project claims it). Explicit targets bypass
	// this function entirely — a human naming a seat outranks the
	// project boundary.
	pool := state.PoolForCwd()
	if len(pool) < 2 {
		return "", errors.New("only one account is available here; nothing to rotate to")
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
	candidates := make([]strategy.Candidate, 0, len(pool))
	for _, a := range pool {
		candidates = append(candidates, strategy.Candidate{Email: a.Email, Slot: a.Slot, CacheKey: a.CacheKey()})
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
		return pick.Identifier(), nil
	}
	return rotateFallback(state, cache, cfg)
}

// rotateFallback walks store's rotation order, but still refuses accounts
// that are known to have no capacity. Missing usage is treated as usable so
// fresh installs can rotate before the first refresh completes.
func rotateFallback(state *store.State, cache usage.Cache, cfg *config.Config) (string, error) {
	pool := state.PoolForCwd()
	for _, slot := range rotationSlots(state) {
		acct, ok := pool[slot]
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

const (
	retryBaseDelay = 10 * time.Second
	retryMaxDelay  = 2 * time.Minute
)

// fibonacciDelay returns the wait before auto-continue attempt n
// (0-based): 10s, 10s, 20s, 30s, 50s, 80s, then capped at 2 minutes.
// The clock starts only after StopFailure fires — Claude Code has
// already burned through its own internal retries by then — so there
// is no reason to start slower.
func fibonacciDelay(n int) time.Duration {
	a, b := 1, 1
	for i := 0; i < n; i++ {
		a, b = b, a+b
		if time.Duration(a)*retryBaseDelay >= retryMaxDelay {
			return retryMaxDelay
		}
	}
	return time.Duration(a) * retryBaseDelay
}

// snippet trims an error message down to a single displayable line.
func snippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

// resumableSession decides whether the wrapper can relaunch into an
// existing session after a swap, and which session id to use.
// sessionID is what the SessionStart hook reported for the run that
// just exited; argv is that run's launch argv.
//
// A completed turn (hadTurns) is the usual proof a transcript exists —
// an empty, just-started session has nothing to resume and claude
// rejects --resume for it. But a run that was itself resuming an
// earlier session has a transcript by definition, even when it died
// before completing a new turn (e.g. a swap landing on an account that
// rate-limits during the very first replayed prompt). Falling back to
// the original argv in that case is what stranded unattended runs: a
// bare `--resume` in it opens the interactive session picker, which
// nobody is around to answer at 3am.
func resumableSession(sessionID string, argv []string, autoResume, hadTurns bool) (string, bool) {
	if !autoResume {
		return "", false
	}
	sid := sessionID
	if sid == "" {
		if v := resumeSessionID(argv); v != "" && !strings.HasPrefix(v, "-") {
			sid = v
		}
	}
	if sid == "" {
		return "", false
	}
	if hadTurns || isResumeArgv(argv) {
		return sid, true
	}
	return "", false
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
