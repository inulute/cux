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

	pollInterval     = 100 * time.Millisecond
	gracefulExitWait = 5 * time.Second
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
	if err := writeWrapperPID(pid); err != nil {
		fmt.Fprintf(w, "cux: warning: cannot publish wrapper pid: %v\n", err)
	}
	defer cleanupWrapperPID(pid)

	// One-shot background refresh so threshold checks have something
	// to work with on the first turn. Errors are ignored — a fresh
	// install with no usage data is fine; threshold logic falls back
	// to "no decision" rather than guessing.
	go func() { _, _ = monitor.RefreshAll() }()

	// lastManualTarget holds the email the user explicitly switched to
	// within this wrapper session. Threshold auto-switch is suppressed
	// while the live account matches this value, so a manual choice is
	// not silently undone by usage-based rotation.
	var lastManualTarget string

	currentArgv := argv
	for {
		exitCode, sessionID, hadTurns, p, err := launch(claudeBin, currentArgv, pid, &cfg, lastManualTarget, w)
		if err != nil {
			return exitCode, err
		}
		if p == nil {
			// No swap pending ⇒ user quit normally.
			return exitCode, nil
		}

		target, err := resolveTarget(p.explicitTarget, p.trigger, &cfg)
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

		canResume := sessionID != "" && cfg.AutoResume && (hadTurns || p.trigger == history.TriggerManual)
		if canResume {
			// Resume the existing session on the new account. For manual
			// /switch we resume even when no turns have completed yet —
			// the user explicitly requested a switch and expects to land
			// back in the same session, not the welcome screen.
			// For rate-limit and threshold swaps we require hadTurns so
			// we don't pass --resume for an empty session that claude would reject.
			switch p.trigger {
			case history.TriggerRateLimit:
				fmt.Fprintf(w, "cux: rate limit on %s → swapped to %s, resuming…\n", from.Email, to.Email)
			case history.TriggerManual:
				fmt.Fprintf(w, "cux: %s → %s, resuming…\n", from.Email, to.Email)
			default:
				fmt.Fprintf(w, "cux: %s → %s (%s), resuming…\n", from.Email, to.Email, p.reason)
			}
			currentArgv = []string{"--resume", sessionID}
			if p.resumeMessage != "" {
				currentArgv = append(currentArgv, p.resumeMessage)
			} else if cfg.AutoMessage != "" {
				currentArgv = append(currentArgv, cfg.AutoMessage)
			}
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
	if _, ok, _ := signals.Read(wrapperPID, signals.Stopped); ok {
		_ = signals.Consume(wrapperPID, signals.Stopped)
		hadTurns.Store(true)
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
	email, err := switcher.CurrentLiveEmail()
	if err != nil {
		return usage.AccountUsage{}
	}
	cache, err := usage.LoadCache()
	if err != nil {
		return usage.AccountUsage{}
	}
	return cache[email]
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
	cache, err := usage.LoadCache()
	if err != nil || cache == nil {
		return nil
	}
	u, ok := cache[email]
	if !ok {
		return nil
	}
	over, reason := usage.IsOverThreshold(u, cfg.Thresholds)
	if !over {
		return nil
	}
	state, err := store.Load()
	if err != nil {
		return nil
	}
	candidates := make([]strategy.Candidate, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		candidates = append(candidates, strategy.Candidate{Email: a.Email})
	}
	pick, ok := strategy.PickNext(cfg.ResolvedStrategy(), cfg.Strategy.Order, candidates,
		strategy.Candidate{Email: email}, cache, cfg.Thresholds)
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

// gracefulExit asks the child to shut down. We send the OS-appropriate
// "interrupt" signal first (matches what Ctrl-C would do), wait briefly
// for claude to flush and exit on its own, then escalate to a hard
// terminate if it's still alive.
//
// Because gracefulExit only runs after a Stop signal, the child is at
// "waiting for user input" and the transcript is already flushed. The
// race v0.1 had to warn about doesn't exist here.
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
//  3. Bare rotation (NextInRotation) as a last-resort fallback when
//     strategy returns no candidate, e.g. on a fresh install with no
//     usage data and Manual mode.
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

	// Try strategy first.
	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	candidates := make([]strategy.Candidate, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		candidates = append(candidates, strategy.Candidate{Email: a.Email})
	}
	// In manual mode strategy.PickNext returns ok=false, but a manual
	// trigger with no explicit target still means "rotate" — fall
	// through to bare rotation in that case.
	kind := cfg.ResolvedStrategy()
	if trigger == history.TriggerManual && kind == strategy.KindManual {
		return rotateFallback(state)
	}
	if pick, ok := strategy.PickNext(kind, cfg.Strategy.Order, candidates,
		strategy.Candidate{Email: current}, cache, cfg.Thresholds); ok {
		return pick.Email, nil
	}
	return rotateFallback(state)
}

// rotateFallback picks "any other account" using store's existing
// rotation helper. Used when strategy declines (manual mode) or when
// usage data is absent.
func rotateFallback(state *store.State) (string, error) {
	next := state.NextInRotation(state.ActiveSlot)
	if next == 0 {
		for slot := range state.Accounts {
			if slot != state.ActiveSlot {
				next = slot
				break
			}
		}
	}
	if next == 0 {
		return "", errors.New("could not determine a rotation target")
	}
	return strconv.Itoa(next), nil
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
