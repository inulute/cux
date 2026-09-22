package wrapper

// SlashSwitch implements `cux __slash-switch <target>`, the body the
// /switch and /cux:switch slash commands shell out to.
//
// The swap happens in place: this process rewrites the live credential
// blob and returns. Claude Code re-reads credentials on every API
// request, so the very next message continues on the new account in the
// same session — no kill, no `--resume`, no lost context. (The auto
// threshold hook uses the same in-place mechanism.) Mid-turn rate-limit
// recovery and `cux force-switch` still go through the wrapper's
// kill+resume path, which reloads the transcript when a turn is broken.
import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/usage"
)

// SlashSwitch is invoked by the slash command's bash block. target may
// be empty — that means "rotate per the configured strategy".
func SlashSwitch(target string, w io.Writer) error {
	if os.Getenv(envWrapped) != "1" {
		return errors.New("/switch requires cux as the entry point — start your session with `cux` instead of `claude`")
	}

	pidStr := os.Getenv(envWrapperPID)
	if pidStr == "" {
		return errors.New("CUX_WRAPPER_PID not set; cannot route switch")
	}
	wrapperPID, err := strconv.Atoi(pidStr)
	if err != nil || wrapperPID <= 0 {
		return fmt.Errorf("invalid CUX_WRAPPER_PID: %q", pidStr)
	}

	target = strings.TrimSpace(target)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	state, err := store.Load()
	if err != nil {
		return err
	}
	if len(state.Accounts) < 2 {
		return errors.New("need at least two managed accounts — run `cux add` after logging into another account")
	}

	// Resolve explicit target (slot/email/alias) or rotate per strategy.
	resolved, err := resolveTarget(target, history.TriggerManual, &cfg, nil)
	if err != nil {
		return err
	}
	acct, err := state.Resolve(resolved)
	if err != nil {
		return err
	}
	if acct.Slot == state.ActiveSlot {
		fmt.Fprintf(w, "cux: already on %s, nothing to do\n", acct.Email)
		return nil
	}

	// In-place swap: rewrite the live credential blob. Claude picks it up
	// on its next API request, so the same session continues on the new
	// account with no restart.
	from, to, err := switcher.SwitchTo(resolved)
	if err != nil {
		return fmt.Errorf("switch failed: %w", err)
	}

	// Record the swap so `cux history` and `cux list` stay accurate.
	cwd, _ := os.Getwd()
	cache, _ := usage.LoadCache()
	fromU := cache[from.CacheKey()]
	toU := cache[to.CacheKey()]
	_ = history.Append(history.Entry{
		From:        from.Email,
		To:          to.Email,
		Trigger:     history.TriggerManual,
		Reason:      "user requested via /switch",
		CWD:         cwd,
		FromUsage5h: utilizationOrZero(fromU.FiveHour),
		FromUsage7d: utilizationOrZero(fromU.SevenDay),
		ToUsage5h:   utilizationOrZero(toU.FiveHour),
		ToUsage7d:   utilizationOrZero(toU.SevenDay),
	})

	// Honour the deliberate choice: skip the auto threshold check on the
	// very next prompt so a `/switch` onto a busy account is not
	// immediately undone by auto-switch. (setManualSwitchState alone does
	// not gate the UserPromptSubmit hook — the replay flag does.)
	setManualSwitchState(to.Email)
	_ = os.WriteFile(paths.ReplayFlagFile(wrapperPID), []byte("1"), 0o600)

	// Freshen both accounts' usage in the background for `cux list`.
	go func(fromEmail, toEmail string) {
		_ = monitor.RefreshActive(fromEmail)
		_ = monitor.RefreshActive(toEmail)
	}(from.Email, to.Email)

	fmt.Fprint(w, renderSwitched(from, to, toU))
	return nil
}

// renderSwitched is what the user reads instead of their prompt. Claude Code
// prefixes a blocked prompt with "operation blocked by hook", so this has to
// state plainly that the switch happened and that nothing was lost — the
// prefix otherwise makes a success look like a failure.
func renderSwitched(from, to store.Account, u usage.AccountUsage) string {
	const width = 74
	var b strings.Builder
	line := func(label, value string) {
		b.WriteString(fmt.Sprintf("│ %-9s %-*s │\n", label, width-10, clipTo(value, width-10)))
	}
	rule := strings.Repeat("─", width+2)

	b.WriteString(":: A C C O U N T   S W I T C H E D ::\n\n")
	b.WriteString("┌" + rule + "┐\n")
	line("NOW LIVE", to.Email+slotSuffix(to))
	line("PREVIOUS", from.Email+slotSuffix(from))
	if head := headroom(u); head != "" {
		line("HEADROOM", head)
	}
	line("SESSION", "kept — credentials swapped in place, nothing restarted")
	b.WriteString("└" + rule + "┘\n")
	return b.String()
}

func slotSuffix(a store.Account) string {
	if a.Slot == 0 {
		return ""
	}
	return fmt.Sprintf("  [slot %02d]", a.Slot)
}

// headroom reports what is left on the seat just switched to, so the user can
// see straight away whether the swap actually bought them anything.
func headroom(u usage.AccountUsage) string {
	parts := make([]string, 0, 2)
	if u.FiveHour != nil {
		parts = append(parts, fmt.Sprintf("5h %.0f%% used", u.FiveHour.Utilization))
	}
	if u.SevenDay != nil {
		parts = append(parts, fmt.Sprintf("7d %.0f%% used", u.SevenDay.Utilization))
	}
	return strings.Join(parts, "   ")
}

func clipTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// ForceSwitch is the out-of-band version of SlashSwitch. It is meant
// for the hard-limit state where Claude accepts keyboard input but does
// not dispatch custom slash commands. A second terminal can run
// `cux force-switch [target]`; the active wrapper sees the same signal
// that /switch would have written.
func ForceSwitch(target string, w io.Writer) error {
	b, err := os.ReadFile(paths.ClaudePIDFile())
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no active cux wrapper found — start Claude with `cux` first")
		}
		return err
	}
	wrapperPID, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || wrapperPID <= 0 {
		return fmt.Errorf("invalid active wrapper pid file: %q", strings.TrimSpace(string(b)))
	}

	target = strings.TrimSpace(target)
	state, err := store.Load()
	if err != nil {
		return err
	}
	if len(state.Accounts) < 2 {
		return errors.New("need at least two managed accounts — run `cux add` after logging into another account")
	}
	if target != "" {
		resolved, err := state.Resolve(target)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "cux: forcing switch to %s in active session\n", resolved.Email)
	} else {
		fmt.Fprintln(w, "cux: forcing rotation in active session")
	}
	return signals.Write(wrapperPID, signals.SwitchRequested, signals.SwitchRequestedPayload{
		Target:    target,
		Timestamp: time.Now().UTC(),
	})
}
