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
	resolved, err := resolveTarget(target, history.TriggerManual, &cfg)
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

	fmt.Fprintf(w, "cux: %s → %s — switched in place; this conversation continues on %s.\n",
		from.Email, to.Email, to.Email)
	return nil
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
