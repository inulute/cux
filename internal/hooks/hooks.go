// Package hooks implements the bodies of `cux hook stop`,
// `cux hook session-start`, and `cux hook rate-limit`.
//
// These subcommands are invoked by Claude Code via entries in
// ~/.claude/settings.json. Their job is small: read the JSON Claude
// Code pipes on stdin, decide whether the event is interesting, and
// emit a signal file the cux wrapper polls for.
//
// All three are gated by the CUX_WRAPPED env var. When unset (the user
// is running `claude` directly, not under `cux`), the hook silently
// no-ops with exit 0 — so installed hooks are harmless when cux is not
// the parent process.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/config"
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

	// Hook timeouts in settings.json are in seconds. Reading stdin
	// shouldn't ever take more than a few hundred ms in practice; we
	// fail fast rather than block claude on a stuck hook.
	stdinReadDeadline = 4 * time.Second
)

// stdinJSON shapes mirror what claude-revolver reads. We tolerate
// missing fields with `omitempty` defaults so a future Claude Code
// version that adds keys does not break us.

type stopHookInput struct {
	SessionID string `json:"session_id"`
}

type sessionStartHookInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd,omitempty"`
	Source    string `json:"source,omitempty"`
}

type rateLimitHookInput struct {
	// error is json.RawMessage so we can handle both string and object
	// shapes that different Claude Code versions may emit.
	Error                json.RawMessage `json:"error,omitempty"`
	ErrorDetails         json.RawMessage `json:"error_details,omitempty"`
	LastAssistantMessage json.RawMessage `json:"last_assistant_message,omitempty"`
	HookEventName        string          `json:"hook_event_name,omitempty"`
}

type userPromptSubmitHookInput struct {
	Prompt string `json:"prompt"`
}

type userPromptExpansionHookInput struct {
	CommandName string `json:"command_name,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
}

type userPromptSubmitHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type sessionStartHookOutput struct {
	HookSpecificOutput sessionStartSpecificOutput `json:"hookSpecificOutput"`
}

type sessionStartSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// UserPromptSubmit is `cux hook prompt-submit`. It intercepts cux's
// own command-like prompts before Claude sends them to the model. This
// is the critical path when the account is already hard-limited:
// prompt-based slash commands may not expand, but hooks still run
// before prompt processing.
func UserPromptSubmit(stdin io.Reader, stdout io.Writer) error {
	if !isWrapped() {
		return nil
	}
	var in userPromptSubmitHookInput
	if err := decode(stdin, &in); err != nil {
		return nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil
	}
	if prompt == "/switch" || strings.HasPrefix(prompt, "/switch ") {
		target := strings.TrimSpace(strings.TrimPrefix(prompt, "/switch"))
		writePromptSwitch(target, stdout)
		return nil
	}
	if handled, err := handleCuxPromptCommand(prompt, stdout); handled || err != nil {
		return err
	}
	if handled, err := handleAutoSwitchPrompt(prompt, stdout); handled || err != nil {
		return err
	}
	return nil
}

func UserPromptExpansion(stdin io.Reader, stdout io.Writer) error {
	if !isWrapped() {
		return nil
	}
	var in userPromptExpansionHookInput
	if err := decode(stdin, &in); err != nil {
		return nil
	}
	cmd := strings.TrimPrefix(strings.TrimSpace(in.CommandName), "/")
	prompt := strings.TrimSpace(in.Prompt)
	if cmd != "rate-limit-options" && prompt != "/rate-limit-options" {
		return nil
	}

	if handled, err := handleAutoSwitchPrompt("/rate-limit-options", stdout); handled || err != nil {
		return nil
	}

	if msg, err := renderPromptUsage(true); err == nil {
		writePromptBlock(stdout, msg)
		return nil
	}
	writePromptBlock(stdout, "cux: session limit reached; no usable managed account is available right now.")
	return nil
}

func handleAutoSwitchPrompt(prompt string, stdout io.Writer) (bool, error) {
	// Consume the replay flag written by the wrapper before relaunching.
	// This prevents triggering another switch when the replayed prompt
	// lands on an account that is also near threshold.
	isReplay := consumeReplayFlag()

	cfg, err := config.Load()
	if err != nil || !cfg.AutoSwitchOnThreshold {
		return false, nil
	}
	email, err := switcher.CurrentLiveEmail()
	if err != nil {
		return false, nil
	}
	// Use the real cache key (OrgUUID when present, email otherwise).
	// Accounts in an org are keyed by OrgUUID in the usage cache, so a
	// bare email lookup silently misses and the threshold check never fires.
	cacheKey, err := switcher.CurrentLiveCacheKey()
	if err != nil {
		return false, nil
	}

	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	u, uOK := cachedUsage(cache, cacheKey, email)
	if !uOK {
		return false, nil
	}
	over, why := usage.IsOverThreshold(u, cfg.Thresholds)
	if !over {
		return false, nil
	}

	// Refresh all accounts before picking a target. The cache may be stale
	// (e.g. a candidate's 5h window has reset since the last poll) and we
	// must not switch to an account that looks exhausted only because the
	// cache hasn't caught up yet. The hook timeout is 20 s so a refresh
	// here is safe.
	if fresh, _ := monitor.RefreshAll(); fresh != nil {
		cache = fresh
		// Re-read the current account from the fresh cache.
		if freshU, ok := cachedUsage(fresh, cacheKey, email); ok {
			u = freshU
			// If the current account itself has recovered (e.g. its 5h window
			// just reset), don't switch — let the prompt through.
			if newOver, _ := usage.IsOverThreshold(u, cfg.Thresholds); !newOver {
				return false, nil
			}
		}
	}

	state, err := store.Load()
	if err != nil {
		return false, nil
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
	pick, picked := strategy.PickNext(cfg.ResolvedStrategy(), cfg.Strategy.Order, candidates,
		current, cache, cfg.Thresholds)

	if picked && !isReplay {
		// /rate-limit-options is Claude Code's internally-issued slash command
		// when a session limit fires mid-turn (during tool use). In that case
		// the turn is already broken — we need the wrapper's kill+resume flow
		// to reload the session transcript on the new account. Signal the
		// wrapper and block the command so the menu never appears.
		if strings.HasPrefix(strings.TrimSpace(prompt), "/rate-limit-options") {
			pid, err := wrapperPID()
			if err != nil {
				// Not running under the wrapper — fall through to in-place swap.
				goto inPlaceSwap
			}
			if err := signals.Write(pid, signals.SwitchRequested, signals.SwitchRequestedPayload{
				Target:    pick.Identifier(),
				Timestamp: time.Now().UTC(),
			}); err != nil {
				writePromptBlock(stdout, fmt.Sprintf("cux: %s\ncux: signal failed: %v", why, err))
				return true, nil
			}
			writePromptBlock(stdout, fmt.Sprintf("cux: %s\ncux: → switching to %s and resuming your session...", why, pick.Email))
			return true, nil
		}

	inPlaceSwap:
		// Pre-turn prompt: swap credentials in-place. Claude Code reads
		// credentials on every API request, so this prompt will be processed
		// by the new account with no restart or manual resend needed.
		from, to, switchErr := switcher.SwitchTo(pick.Identifier())
		if switchErr != nil {
			writePromptBlock(stdout, fmt.Sprintf("cux: %s\ncux: failed to switch to %s: %v", why, pick.Email, switchErr))
			return true, nil
		}
		// Notify on stderr — visible in the terminal even inside the TUI.
		// Writing nothing to stdout causes Claude Code to approve the prompt,
		// so it continues on the new account without any manual resend.
		fmt.Fprintf(os.Stderr, "\ncux: %s → %s (%s)\n", from.Email, to.Email, why)
		return true, nil
	}

	if isReplay {
		// Leftover replay flag from the old wrapper-based switch path.
		// Let the prompt through — auto-switch re-engages on the next prompt.
		return false, nil
	}

	// No usable account found. Show an actionable warning.
	var b strings.Builder
	b.WriteString("cux: " + why + "\n")
	b.WriteString("cux: all managed accounts are at or above the usage threshold\n")

	// Only suggest raising the threshold when accounts are throttled by it
	// (not at 100%). At 100% the threshold is irrelevant — only a reset helps.
	trulyExhausted := (u.FiveHour != nil && u.FiveHour.Utilization >= 100) ||
		(u.SevenDay != nil && u.SevenDay.Utilization >= 100)
	if !trulyExhausted {
		threshold := cfg.Thresholds.FiveHour
		if threshold == 0 {
			threshold = 90
		}
		suggested := threshold + 10
		if suggested > 95 {
			suggested = 95
		}
		b.WriteString(fmt.Sprintf("cux: threshold is %d%% — raise it with:  /cux:config set thresholds.five_hour %d\n",
			threshold, suggested))
	}

	if _, resetEmail, reset, ok := nextResetSlot(state, cache); ok {
		b.WriteString(fmt.Sprintf("cux: next reset: %s in %s\n", resetEmail, reset))
	}
	b.WriteString("\nResend your prompt after adjusting the threshold or waiting for reset.")
	writePromptBlock(stdout, strings.TrimRight(b.String(), "\n"))
	return true, nil
}

func consumeReplayFlag() bool {
	pid, err := wrapperPID()
	if err != nil {
		return false
	}
	flagFile := paths.ReplayFlagFile(pid)
	if _, err := os.Stat(flagFile); err == nil {
		_ = os.Remove(flagFile)
		return true
	}
	return false
}

func handleCuxPromptCommand(prompt string, stdout io.Writer) (bool, error) {
	if !strings.HasPrefix(prompt, "/cux:") {
		return false, nil
	}
	fields := strings.Fields(strings.TrimPrefix(prompt, "/cux:"))
	if len(fields) == 0 {
		writePromptBlock(stdout, "cux: missing command after /cux:")
		return true, nil
	}

	var args []string
	switch fields[0] {
	case "add":
		args = append([]string{"add"}, fields[1:]...)
	case "switch":
		target := strings.TrimSpace(strings.TrimPrefix(prompt, "/cux:switch"))
		writePromptSwitch(target, stdout)
		return true, nil
	case "list":
		refresh := hasArg(fields[1:], "--refresh")
		text, err := renderPromptUsage(refresh)
		if err != nil {
			writePromptBlock(stdout, "cux: "+err.Error())
			return true, nil
		}
		writePromptBlock(stdout, text)
		return true, nil
	case "status":
		text, err := renderPromptUsage(true)
		if err != nil {
			writePromptBlock(stdout, "cux: "+err.Error())
			return true, nil
		}
		writePromptBlock(stdout, text)
		return true, nil
	case "support":
		writePromptBlock(stdout, renderPromptSupport())
		return true, nil
	case "remove":
		args = append([]string{"remove"}, fields[1:]...)
	case "config":
		text, err := renderPromptConfig(fields[1:])
		if err != nil {
			writePromptBlock(stdout, "cux: "+err.Error())
			return true, nil
		}
		writePromptBlock(stdout, text)
		return true, nil
	case "usage-refresh":
		text, err := renderPromptUsage(true)
		if err != nil {
			writePromptBlock(stdout, "cux: "+err.Error())
			return true, nil
		}
		writePromptBlock(stdout, text)
		return true, nil
	case "help":
		writePromptBlock(stdout, "cux commands: /switch, /cux:switch, /cux:add, /cux:list, /cux:status, /cux:support, /cux:remove, /cux:config, /cux:usage-refresh")
		return true, nil
	default:
		writePromptBlock(stdout, "cux: unknown /cux command "+fields[0])
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cux", args...)
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if ctx.Err() == context.DeadlineExceeded {
		text = "cux: command timed out"
	} else if err != nil && text == "" {
		text = "cux: " + err.Error()
	} else if err != nil {
		text = text + "\n" + "cux: command failed: " + err.Error()
	} else if text == "" {
		text = "cux: done"
	}
	writePromptBlock(stdout, text)
	return true, nil
}

func writePromptSwitch(target string, stdout io.Writer) {
	if strings.TrimSpace(target) == "" {
		if ok, text := promptSwitchHasTarget(); !ok {
			writePromptBlock(stdout, text)
			return
		}
	}
	pid, err := wrapperPID()
	if err != nil {
		writePromptBlock(stdout, "cux: "+err.Error())
		return
	}
	if err := signals.Write(pid, signals.SwitchRequested, signals.SwitchRequestedPayload{
		Target:    strings.TrimSpace(target),
		Timestamp: time.Now().UTC(),
	}); err != nil {
		writePromptBlock(stdout, "cux: "+err.Error())
		return
	}
	reason := "cux: switching accounts..."
	if strings.TrimSpace(target) != "" {
		reason = "cux: switching accounts to " + strings.TrimSpace(target) + "..."
	}
	writePromptBlock(stdout, reason)
}

func promptSwitchHasTarget() (bool, string) {
	cfg, err := config.Load()
	if err != nil {
		return true, ""
	}
	state, err := store.Load()
	if err != nil || len(state.Accounts) < 2 {
		return true, ""
	}
	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	current, _ := switcher.CurrentLiveEmail()
	pool := state.PoolForCwd()
	candidates := make([]strategy.Candidate, 0, len(pool))
	for _, a := range pool {
		candidates = append(candidates, strategy.Candidate{Email: a.Email})
	}
	if _, ok := strategy.PickNext(cfg.ResolvedStrategy(), cfg.Strategy.Order, candidates,
		strategy.Candidate{Email: current}, cache, cfg.Thresholds); ok {
		return true, ""
	}
	for _, slot := range state.SortedSlots() {
		acct, inPool := pool[slot]
		if !inPool {
			continue
		}
		if slot != state.ActiveSlot && accountHasPromptCapacity(cache, acct, cfg.Thresholds) {
			return true, ""
		}
	}
	text, err := renderPromptUsage(false)
	if err != nil {
		return false, "cux: no usable accounts available; all managed accounts are exhausted or need login"
	}
	return false, text
}

func renderPromptUsage(refresh bool) (string, error) {
	state, err := store.Load()
	if err != nil {
		return "", err
	}
	if len(state.Accounts) == 0 {
		return "CUX\n\nNo managed accounts yet. Use /cux:add after logging in.", nil
	}

	var warnings []string
	if refresh {
		_, errs := monitor.RefreshAll()
		for _, e := range errs {
			warnings = append(warnings, e.Error())
		}
	}

	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	cfg, _ := config.Load()
	liveEmail, _ := switcher.CurrentLiveEmail()
	activeSlot := 0
	activeEmail := "(unknown)"
	if state.ActiveSlot != 0 {
		if acct, ok := state.Accounts[state.ActiveSlot]; ok {
			activeSlot = acct.Slot
			activeEmail = acct.Email
		}
	}

	var b strings.Builder
	b.WriteString(":: A C C O U N T   P O O L ::\n\n")
	b.WriteString("┌──────────────────────────────────────────────────────────────────────────────────────────────────┐\n")
	b.WriteString(fmt.Sprintf("│  %-52s %-42s │\n",
		fmt.Sprintf("SYSTEM STATUS : ACTIVE [%02d]", activeSlot),
		fmt.Sprintf("MANAGED ACCOUNTS : %02d", len(state.Accounts)),
	))
	liveLabel := activeEmail
	if liveEmail != "" {
		liveLabel = liveEmail
	}
	b.WriteString(fmt.Sprintf("│  %-96s│\n", clipDisplay("LIVE INSTANCE : "+liveLabel, 96)))
	if refresh {
		b.WriteString(fmt.Sprintf("│  %-96s│\n", "USAGE SNAPSHOT : REFRESHED NOW"))
	}
	b.WriteString("└──────────────────────────────────────────────────────────────────────────────────────────────────┘\n\n")
	b.WriteString("┌──────┬───────────────────────────┬────────┬──────────────────────┬──────────────────────┬────────┐\n")
	b.WriteString("│ SLOT │ ACCOUNT                   │ STATE  │ 5H USAGE             │ 7D USAGE             │ RESET  │\n")
	b.WriteString("├──────┼───────────────────────────┼────────┼──────────────────────┼──────────────────────┼────────┤\n")

	anyUsable := false
	slots := state.SortedSlots()
	sort.Ints(slots)
	for i, slot := range slots {
		acct := state.Accounts[slot]
		u, _ := cachedUsage(cache, acct.CacheKey(), acct.Email)
		stateLabel := ""
		if acct.Email == liveEmail {
			stateLabel = "active"
		}
		if u.TokenExpired {
			if stateLabel == "" {
				stateLabel = "expired"
			} else {
				stateLabel += "+expired"
			}
		}
		if accountHasPromptCapacity(cache, acct, cfg.Thresholds) {
			anyUsable = true
		}
		if stateLabel == "" {
			stateLabel = capacityLabel(u, cfg.Thresholds)
		}
		b.WriteString(renderAccountRow(slot, acct.Email, u, stateLabel))
		if i != len(slots)-1 {
			b.WriteString("│      │                           │        │                      │                      │        │\n")
		}
	}
	b.WriteString("└──────┴───────────────────────────┴────────┴──────────────────────┴──────────────────────┴────────┘")
	if !anyUsable {
		b.WriteString("\n\nSTATUS : ALL MANAGED ACCOUNTS EXHAUSTED")
		if slot, email, reset, ok := nextResetSlot(state, cache); ok {
			b.WriteString(fmt.Sprintf("\nNEXT RESET : SLOT [%02d] %s  IN %s", slot, email, reset))
		}
		b.WriteString("\nACTION : WAIT FOR RESET OR ADD ANOTHER ACCOUNT")
	} else if slot, email, reset, ok := nextUsableSlot(state, cache, cfg.Thresholds); ok {
		b.WriteString(fmt.Sprintf("\n\nNEXT USABLE : SLOT [%02d] %s", slot, email))
		if reset != "" {
			b.WriteString("  RESET " + reset)
		}
	}
	for _, warning := range warnings {
		b.WriteString("\n\nWARNING: ")
		b.WriteString(warning)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderPromptConfig(args []string) (string, error) {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show", "keys":
		c, err := config.Load()
		if err != nil {
			return "", err
		}
		keys := config.Keys(c)
		var b strings.Builder
		b.WriteString(":: C U X   S E T T I N G S ::\n\n")
		b.WriteString("┌─────────────────────────────┬──────────────────────┬────────────────────────────────────────────┐\n")
		b.WriteString("│ KEY                         │ VALUE                │ NOTES                                      │\n")
		b.WriteString("├─────────────────────────────┼──────────────────────┼────────────────────────────────────────────┤\n")
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("│ %-27s │ %-20s │ %-42s │\n",
				clipDisplay(k.Key, 27),
				clipDisplay(displayConfigValue(k.Current), 20),
				clipDisplay(k.Description, 42),
			))
		}
		b.WriteString("└─────────────────────────────┴──────────────────────┴────────────────────────────────────────────┘\n\n")
		b.WriteString("SET     /cux:config set [key] [value]\n")
		b.WriteString("EDIT    run `cux config edit` in your terminal for the interactive settings UI\n")
		b.WriteString("EXAMPLE  /cux:config set strategy.kind balanced")
		return b.String(), nil
	case "edit":
		return ":: C U X   S E T T I N G S ::\n\nInteractive settings editor is terminal-only.\nRun this outside Claude:\n\n  cux config edit", nil
	case "set":
		if len(args) != 3 {
			return "", fmt.Errorf("usage: /cux:config set <key> <value>")
		}
		c, err := config.Load()
		if err != nil {
			return "", err
		}
		c, err = config.Set(c, args[1], args[2])
		if err != nil {
			return "", err
		}
		if err := config.Save(c); err != nil {
			return "", err
		}
		return fmt.Sprintf(":: C U X   S E T T I N G S ::\n\nUPDATED  %s = %s", args[1], args[2]), nil
	default:
		return "", fmt.Errorf("usage: /cux:config show | keys | set <key> <value>")
	}
}

func renderPromptSupport() string {
	return "CUX SUPPORT\n\nSupport cux development:\nhttps://support.inulute.com"
}

func displayConfigValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}

func renderAccountRow(slot int, email string, u usage.AccountUsage, stateLabel string) string {
	line := fmt.Sprintf("│ %-4s │ %-25s │ %-6s │ %-20s │ %-20s │ %-6s │\n",
		fmt.Sprintf("%02d", slot),
		clipDisplay(email, 25),
		clipDisplay(strings.ToUpper(stateLabel), 6),
		usageBlock(u.FiveHour),
		usageBlock(u.SevenDay),
		clipDisplay(resetForAccount(u), 6),
	)
	if u.TokenExpired {
		line += fmt.Sprintf("│ %-96s │\n", "TOKEN EXPIRED: RE-LOGIN AND RUN /cux:add")
	}
	return line
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func usageCell(w *usage.Window) string {
	if w == nil {
		return "[----------] --"
	}
	pct := w.Utilization
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	full := int((pct + 9) / 10)
	if full > 10 {
		full = 10
	}
	return fmt.Sprintf("[%s%s] %3.0f%%", strings.Repeat("#", full), strings.Repeat("-", 10-full), w.Utilization)
}

func usageBar(w *usage.Window) string {
	if w == nil {
		return "[----------]   --"
	}
	pct := w.Utilization
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	full := int((pct + 9) / 10)
	if full > 10 {
		full = 10
	}
	return fmt.Sprintf("[%s%s] %3.0f%%", strings.Repeat("=", full), strings.Repeat(".", 10-full), w.Utilization)
}

func usageBlock(w *usage.Window) string {
	if w == nil {
		return "░░░░░░░░░░░░░░░  --"
	}
	pct := w.Utilization
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	full := int((pct + 6.666) / 6.667)
	if full > 15 {
		full = 15
	}
	return fmt.Sprintf("%s%s %3.0f%%", strings.Repeat("█", full), strings.Repeat("░", 15-full), w.Utilization)
}

func resetForWindow(w *usage.Window) string {
	if w == nil || w.ResetsAt == nil {
		return "--"
	}
	return shortDuration(time.Until(*w.ResetsAt))
}

// resetForAccount returns the reset time of the binding capacity window.
// When 7D is hard-full it is the binding constraint (5H resetting does not
// restore capacity), so we surface the 7D reset time instead of 5H.
func resetForAccount(u usage.AccountUsage) string {
	if windowFull(u.SevenDay) {
		return resetForWindow(u.SevenDay)
	}
	return resetForWindow(u.FiveHour)
}

func capacityLabel(u usage.AccountUsage, thresholds usage.Thresholds) string {
	if u.TokenExpired {
		return "expired"
	}
	if !usageHasPromptCapacity(u, thresholds) {
		return "full"
	}
	return "usable"
}

func nextUsableSlot(state *store.State, cache usage.Cache, thresholds usage.Thresholds) (slot int, email, reset string, ok bool) {
	slots := state.SortedSlots()
	sort.Ints(slots)
	for _, s := range slots {
		acct := state.Accounts[s]
		if !accountHasPromptCapacity(cache, acct, thresholds) {
			continue
		}
		u, _ := cachedUsage(cache, acct.CacheKey(), acct.Email)
		return s, acct.Email, resetForWindow(u.FiveHour), true
	}
	return 0, "", "", false
}

func accountHasPromptCapacity(cache usage.Cache, acct store.Account, thresholds usage.Thresholds) bool {
	u, ok := cachedUsage(cache, acct.CacheKey(), acct.Email)
	if !ok {
		return true
	}
	return usageHasPromptCapacity(u, thresholds)
}

func usageHasPromptCapacity(u usage.AccountUsage, thresholds usage.Thresholds) bool {
	if u.TokenExpired {
		return false
	}
	if u.SevenDay != nil && u.SevenDay.Utilization >= 100 {
		return false
	}
	cap5 := thresholds.FiveHour
	if cap5 == 0 || cap5 == 100 {
		cap5 = 90
	}
	return u.FiveHour == nil || u.FiveHour.Utilization < float64(cap5)
}

func nextResetSlot(state *store.State, cache usage.Cache) (slot int, email, reset string, ok bool) {
	var best *time.Time
	bestSlot := 0
	bestEmail := ""
	slots := state.SortedSlots()
	sort.Ints(slots)
	for _, s := range slots {
		acct := state.Accounts[s]
		u, _ := cachedUsage(cache, acct.CacheKey(), acct.Email)
		if u.FiveHour == nil || u.FiveHour.ResetsAt == nil {
			continue
		}
		if best == nil || u.FiveHour.ResetsAt.Before(*best) {
			t := *u.FiveHour.ResetsAt
			best = &t
			bestSlot = s
			bestEmail = acct.Email
		}
	}
	if best == nil {
		return 0, "", "", false
	}
	return bestSlot, bestEmail, shortDuration(time.Until(*best)), true
}

func windowFull(w *usage.Window) bool {
	return w != nil && w.Utilization >= 100
}

func nextPromptReset(u usage.AccountUsage) string {
	var soonest *time.Time
	for _, w := range []*usage.Window{u.FiveHour, u.SevenDay} {
		if w == nil || w.ResetsAt == nil {
			continue
		}
		if soonest == nil || w.ResetsAt.Before(*soonest) {
			t := *w.ResetsAt
			soonest = &t
		}
	}
	if soonest == nil {
		return "--"
	}
	return shortDuration(time.Until(*soonest))
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
		if h < 10 {
			return fmt.Sprintf("%dh %02dm", h, m)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "~"
}

func clipDisplay(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func writePromptBlock(stdout io.Writer, reason string) {
	out, _ := json.Marshal(userPromptSubmitHookOutput{
		Decision: "block",
		Reason:   reason,
	})
	_, _ = stdout.Write(append(out, '\n'))
}

// Stop is `cux hook stop`. Always emits a Stopped signal in wrapped
// mode — the wrapper interprets "received Stop" as "the turn just
// finished and the transcript is flushed; safe to act now."
func Stop(stdin io.Reader) error {
	if !isWrapped() {
		return nil
	}
	pid, err := wrapperPID()
	if err != nil {
		return err
	}
	var in stopHookInput
	_ = decode(stdin, &in) // tolerate missing/empty stdin
	return signals.Write(pid, signals.Stopped, signals.StoppedPayload{
		SessionID: in.SessionID,
		Timestamp: time.Now().UTC(),
	})
}

// SessionStart is `cux hook session-start`. We capture the session ID
// the moment a session begins so the wrapper does not have to fall
// back to mtime-scanning the transcript directory.
func SessionStart(stdin io.Reader, stdout io.Writer) error {
	if !isWrapped() {
		return nil
	}
	pid, err := wrapperPID()
	if err != nil {
		return err
	}
	var in sessionStartHookInput
	_ = decode(stdin, &in)
	if in.SessionID == "" {
		// A session-start with no ID is not useful; do not write the
		// signal. The wrapper will still find the latest .jsonl as a
		// fallback, but this case is rare.
		return nil
	}
	if err := signals.Write(pid, signals.SessionStarted, signals.SessionStartedPayload{
		SessionID: in.SessionID,
		CWD:       in.CWD,
		Source:    in.Source,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if strings.TrimSpace(in.Source) == "resume" {
		writeSessionStartUsageContext(stdout)
	}
	return nil
}

func writeSessionStartUsageContext(stdout io.Writer) {
	text, err := renderPromptUsage(false)
	if err != nil || !strings.Contains(text, "STATUS : ALL MANAGED ACCOUNTS EXHAUSTED") {
		return
	}
	msg := "CUX account status at session resume:\n\n" + text +
		"\n\nAll managed accounts are currently exhausted. You can still read this chat, but new Claude work will wait until the next reset or another account is added."
	out, _ := json.Marshal(sessionStartHookOutput{
		HookSpecificOutput: sessionStartSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: msg,
		},
	})
	_, _ = stdout.Write(append(out, '\n'))
}

// RateLimit is `cux hook rate-limit`. Claude Code routes generic
// PostToolUseFailure events through this; we filter only the "error"
// field for rate-limit indicators before signalling.
//
// We intentionally search only the error field — not tool_input or
// the full payload — to avoid false positives when Claude generates or
// executes code that happens to contain "rate_limit" as a variable
// name or string literal.
//
// The "error" field can be a JSON string or a JSON object depending on
// the Claude Code version; we extract its text from either shape.
func RateLimit(stdin io.Reader) error {
	if !isWrapped() {
		return nil
	}
	pid, err := wrapperPID()
	if err != nil {
		return err
	}

	var in rateLimitHookInput
	if err := decode(stdin, &in); err != nil || len(in.Error) == 0 {
		return nil
	}

	kind, message := classifyFailure(in)
	switch kind {
	case signals.RateLimited:
		return signals.Write(pid, signals.RateLimited, signals.RateLimitedPayload{
			Timestamp: time.Now().UTC(),
			Message:   message,
		})
	case signals.TurnFailed:
		return signals.Write(pid, signals.TurnFailed, signals.TurnFailedPayload{
			Timestamp: time.Now().UTC(),
			Message:   message,
		})
	}
	return nil
}

// classifyFailure decides which signal (if any) a failure event should
// produce, and the message to attach.
//
// Extracts the error text from either JSON shape. StopFailure sends
// error="rate_limit" with optional error_details and last_assistant_message;
// PostToolUseFailure has historically put the useful text in error itself.
// Shape A — string:  "error": "rate limit exceeded"
// Shape B — object:  "error": {"type": "rate_limit_error", "message": "..."}
func classifyFailure(in rateLimitHookInput) (signals.Name, string) {
	errText := extractErrorText(in.Error)
	detailText := extractErrorText(in.ErrorDetails)
	assistantText := extractErrorText(in.LastAssistantMessage)
	combined := strings.Join(nonEmpty(errText, detailText, assistantText), "\n")
	if combined == "" {
		return "", ""
	}

	lower := strings.ToLower(combined)
	isRateLimit := strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "session limit") ||
		strings.Contains(lower, "hit your session limit") ||
		strings.Contains(lower, "usage limit") ||
		// Claude Code's user-facing wording for the usage caps drops the
		// word "usage" in several builds ("You've hit your limit —
		// resets 7pm", "Weekly limit reached"). Missing these turned an
		// exhausted account into an endless fixed-backoff retry loop in
		// the wrapper instead of a swap / sleep-until-reset.
		strings.Contains(lower, "hit your limit") ||
		strings.Contains(lower, "reached your limit") ||
		strings.Contains(lower, "limit reached") ||
		strings.Contains(lower, "limit will reset") ||
		strings.Contains(lower, "overloaded_error") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "429")

	if isRateLimit {
		message := assistantText
		if message == "" {
			message = detailText
		}
		if message == "" {
			message = errText
		}
		return signals.RateLimited, message
	}

	// Non-rate-limit API failure. Two extra guards beyond the rate-limit
	// path, because the failure patterns here (timeout, connection, 5xx)
	// are words that legitimately appear all over normal conversations:
	//
	//   - Only StopFailure qualifies: it means the whole turn died after
	//     Claude Code exhausted its own retries. PostToolUseFailure
	//     routes through here too, but a tool's stderr saying
	//     "connection timed out" is the tool's problem, not the API's.
	//   - Only the structured error fields are searched — never
	//     last_assistant_message. Claude discussing a timeout bug must
	//     not read as the API timing out.
	structuredErr := strings.ToLower(strings.Join(nonEmpty(errText, detailText), "\n"))
	if in.HookEventName == "StopFailure" && isAPIFailure(structuredErr) {
		failMsg := detailText
		if failMsg == "" {
			failMsg = errText
		}
		return signals.TurnFailed, failMsg
	}
	return "", ""
}

// apiStatusCode matches 5xx status codes as standalone tokens so a
// number embedded in unrelated text ("215000 tokens") never counts.
var apiStatusCode = regexp.MustCompile(`\b(500|502|503|504|529)\b`)

// isAPIFailure reports whether an error string from StopFailure looks
// like a transport- or server-side API failure worth retrying, as
// opposed to something the user did (abort) or a model refusal.
func isAPIFailure(lower string) bool {
	for _, pat := range []string{
		"api_error",
		"api error",
		"internal server error",
		"internal_server_error",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"connection error",
		"connection refused",
		"connection reset",
		"connection closed",
		"econnrefused",
		"econnreset",
		"etimedout",
		"enotfound",
		"eai_again",
		"timed out",
		"timeout",
		"network error",
		"fetch failed",
		"socket hang up",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return apiStatusCode.MatchString(lower)
}

// extractErrorText returns a plain string from a json.RawMessage that
// is either a JSON string or a JSON object with "type"/"message" fields.
func extractErrorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string shape first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try object shape.
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Type
	}
	return ""
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
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

// decode reads stdin (with a deadline) and parses it as JSON. Empty
// stdin is treated as an empty object — Claude Code occasionally
// invokes hooks with no body and we should tolerate that quietly.
func decode(r io.Reader, dst interface{}) error {
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		if err != nil {
			ch <- result{err: err}
			return
		}
		if len(b) == 0 {
			ch <- result{err: nil}
			return
		}
		ch <- result{err: json.Unmarshal(b, dst)}
	}()
	select {
	case res := <-ch:
		return res.err
	case <-time.After(stdinReadDeadline):
		return errors.New("hook: stdin read timeout")
	}
}

func isWrapped() bool {
	return os.Getenv(envWrapped) == "1"
}

func wrapperPID() (int, error) {
	v := os.Getenv(envWrapperPID)
	if v == "" {
		return 0, errors.New("hook: CUX_WRAPPER_PID not set")
	}
	pid, err := strconv.Atoi(v)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("hook: bad CUX_WRAPPER_PID %q", v)
	}
	return pid, nil
}
