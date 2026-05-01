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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/monitor"
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
	Error *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

type userPromptSubmitHookInput struct {
	Prompt string `json:"prompt"`
}

type userPromptSubmitHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
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

func handleAutoSwitchPrompt(prompt string, stdout io.Writer) (bool, error) {
	target, reason, ok := promptAutoSwitchTarget()
	if !ok {
		return false, nil
	}
	pid, err := wrapperPID()
	if err != nil {
		return false, err
	}
	if err := signals.Write(pid, signals.SwitchRequested, signals.SwitchRequestedPayload{
		Target:        target,
		ResumeMessage: prompt,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		return false, err
	}
	writePromptBlock(stdout, "cux: "+reason+"\ncux: switching accounts and replaying your prompt after resume...")
	return true, nil
}

func promptAutoSwitchTarget() (target, reason string, ok bool) {
	cfg, err := config.Load()
	if err != nil || !cfg.AutoSwitchOnThreshold {
		return "", "", false
	}
	email, err := switcher.CurrentLiveEmail()
	if err != nil {
		return "", "", false
	}
	cache, err := usage.LoadCache()
	if err != nil || cache == nil {
		return "", "", false
	}
	u, ok := cache[email]
	if !ok {
		return "", "", false
	}
	over, why := usage.IsOverThreshold(u, cfg.Thresholds)
	if !over {
		return "", "", false
	}
	state, err := store.Load()
	if err != nil {
		return "", "", false
	}
	candidates := make([]strategy.Candidate, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		candidates = append(candidates, strategy.Candidate{Email: a.Email})
	}
	pick, picked := strategy.PickNext(cfg.ResolvedStrategy(), cfg.Strategy.Order, candidates,
		strategy.Candidate{Email: email}, cache, cfg.Thresholds)
	if !picked {
		return "", "", false
	}
	return pick.Email, why, true
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
		text, err := renderPromptUsage(false)
		if err != nil {
			writePromptBlock(stdout, "cux: "+err.Error())
			return true, nil
		}
		writePromptBlock(stdout, text)
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
		writePromptBlock(stdout, "cux commands: /switch, /cux:switch, /cux:add, /cux:list, /cux:status, /cux:remove, /cux:config, /cux:usage-refresh")
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

	allFiveHourFull := true
	slots := state.SortedSlots()
	sort.Ints(slots)
	for i, slot := range slots {
		acct := state.Accounts[slot]
		u := cache[acct.Email]
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
		if !windowFull(u.FiveHour) {
			allFiveHourFull = false
		}
		if stateLabel == "" {
			stateLabel = capacityLabel(u)
		}
		b.WriteString(renderAccountRow(slot, acct.Email, u, stateLabel))
		if i != len(slots)-1 {
			b.WriteString("│      │                           │        │                      │                      │        │\n")
		}
	}
	b.WriteString("└──────┴───────────────────────────┴────────┴──────────────────────┴──────────────────────┴────────┘")
	if allFiveHourFull {
		b.WriteString("\n\nSTATUS : NO 5H CAPACITY AVAILABLE")
		if slot, email, reset, ok := nextResetSlot(state, cache); ok {
			b.WriteString(fmt.Sprintf("\nNEXT RESET : SLOT [%02d] %s  IN %s", slot, email, reset))
		}
		b.WriteString("\nACTION : WAIT FOR RESET OR ADD ANOTHER ACCOUNT")
	} else if slot, email, reset, ok := nextUsableSlot(state, cache); ok {
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
		clipDisplay(resetForWindow(u.FiveHour), 6),
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

func capacityLabel(u usage.AccountUsage) string {
	if u.TokenExpired {
		return "expired"
	}
	if windowFull(u.FiveHour) {
		return "full"
	}
	return "usable"
}

func nextUsableSlot(state *store.State, cache usage.Cache) (slot int, email, reset string, ok bool) {
	slots := state.SortedSlots()
	sort.Ints(slots)
	for _, s := range slots {
		acct := state.Accounts[s]
		u := cache[acct.Email]
		if u.TokenExpired || windowFull(u.FiveHour) {
			continue
		}
		return s, acct.Email, resetForWindow(u.FiveHour), true
	}
	return 0, "", "", false
}

func nextResetSlot(state *store.State, cache usage.Cache) (slot int, email, reset string, ok bool) {
	var best *time.Time
	bestSlot := 0
	bestEmail := ""
	slots := state.SortedSlots()
	sort.Ints(slots)
	for _, s := range slots {
		acct := state.Accounts[s]
		u := cache[acct.Email]
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
func SessionStart(stdin io.Reader) error {
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
	return signals.Write(pid, signals.SessionStarted, signals.SessionStartedPayload{
		SessionID: in.SessionID,
		CWD:       in.CWD,
		Source:    in.Source,
		Timestamp: time.Now().UTC(),
	})
}

// RateLimit is `cux hook rate-limit`. Claude Code routes generic
// PostToolUseFailure events through this; we filter the body for
// rate-limit indicators before signalling. False positives here would
// trigger spurious account swaps, so the filter is deliberately
// conservative.
func RateLimit(stdin io.Reader) error {
	if !isWrapped() {
		return nil
	}
	pid, err := wrapperPID()
	if err != nil {
		return err
	}
	var in rateLimitHookInput
	if err := decode(stdin, &in); err != nil {
		return nil // malformed input is not our problem
	}
	if in.Error == nil {
		return nil
	}
	t := strings.ToLower(in.Error.Type)
	m := strings.ToLower(in.Error.Message)
	isRateLimit := strings.Contains(t, "rate_limit") ||
		strings.Contains(m, "rate limit") ||
		strings.Contains(m, "usage limit")
	if !isRateLimit {
		return nil
	}
	return signals.Write(pid, signals.RateLimited, signals.RateLimitedPayload{
		Timestamp: time.Now().UTC(),
		Message:   in.Error.Message,
	})
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
