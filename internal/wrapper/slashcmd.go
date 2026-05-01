package wrapper

// SlashSwitch implements `cux __slash-switch <target>`, the body the
// /switch slash command shells out to.
//
// This writes a switch-requested signal — the wrapper's poll loop picks
// it up, asks Claude to exit cleanly, then performs the swap and resumes
// the session. The command runs locally, so it does not need another
// model turn after the slash command has been accepted.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
)

// SlashSwitch is invoked by the slash command's bash block. target may
// be empty — that means "rotate per the configured strategy", which
// the wrapper resolves once it sees the signal.
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

	// Validate now so the user gets immediate feedback rather than a
	// silent failure 100ms later when the wrapper rejects the target.
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
		if resolved.Slot == state.ActiveSlot {
			fmt.Fprintf(w, "cux: already on %s, nothing to do\n", resolved.Email)
			return nil
		}
		fmt.Fprintf(w, "Switching to %s — reconnecting after this turn ends.\n", resolved.Email)
	} else {
		fmt.Fprintln(w, "Rotating to next account — reconnecting after this turn ends.")
	}

	return signals.Write(wrapperPID, signals.SwitchRequested, signals.SwitchRequestedPayload{
		Target:    target,
		Timestamp: time.Now().UTC(),
	})
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
