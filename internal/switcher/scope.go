package switcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

// A switch has a scope. ScopeAll rewrites the login in the shared Claude
// Code directory (~/.claude unless CLAUDE_CONFIG_DIR says otherwise), so
// every session reading it moves on its next request — one switch used to
// move every running terminal onto the new account. ScopeTerminal moves
// only this process and the children it starts: it points CLAUDE_CONFIG_DIR
// at the seat's own profile, ~/.claude-<alias>, and never writes the shared
// login.
const (
	ScopeTerminal = "terminal"
	ScopeAll      = "all"

	// EnvScope marks a process that runs on a seat's own profile because of
	// a terminal-scoped switch. Anything that would swap the shared login
	// from inside it moves this process instead, or refuses.
	EnvScope = "CUX_SCOPE"
)

// TerminalScoped reports whether this process runs on a seat's own profile
// through a terminal-scoped switch.
func TerminalScoped() bool {
	return os.Getenv(EnvScope) == ScopeTerminal
}

// ParseScopeArgs takes the scope flags out of args: --all/-a,
// --terminal/-t, --scope terminal|all and --scope=terminal|all. scope is ""
// when none was given; rest keeps the other arguments in order.
func ParseScopeArgs(args []string) (rest []string, scope string, err error) {
	set := func(v string) error {
		switch v {
		case ScopeTerminal, ScopeAll:
		default:
			return fmt.Errorf("unknown scope %q (want terminal or all)", v)
		}
		if scope != "" && scope != v {
			return errors.New("conflicting scope flags")
		}
		scope = v
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--all" || a == "-a":
			err = set(ScopeAll)
		case a == "--terminal" || a == "-t":
			err = set(ScopeTerminal)
		case a == "--scope":
			if i+1 >= len(args) {
				return nil, "", errors.New("--scope needs a value: terminal or all")
			}
			i++
			err = set(args[i])
		case strings.HasPrefix(a, "--scope="):
			err = set(strings.TrimPrefix(a, "--scope="))
		default:
			rest = append(rest, a)
		}
		if err != nil {
			return nil, "", err
		}
	}
	return rest, scope, nil
}

// ProfileDir returns the seat's own Claude Code configuration directory,
// $HOME/.claude-<alias>. It must already exist and hold a .claude.json:
// pointing claude at an empty directory would start it with no settings,
// plugins or hooks, and cux never creates Claude Code's config from scratch
// (see claudecfg.WriteOAuthBlock).
func ProfileDir(a store.Account) (string, error) {
	if a.Alias == "" {
		return "", fmt.Errorf("slot %d (%s) has no alias, so it has no profile directory — "+
			"set one with `cux alias %d <name>`, then create ~/.claude-<name>", a.Slot, a.Email, a.Slot)
	}
	dir := filepath.Join(paths.Home(), ".claude-"+a.Alias)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("profile directory %s for slot %d (%s) does not exist — "+
			"create it with `CLAUDE_CONFIG_DIR=%s claude` once", dir, a.Slot, a.Email, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil {
		return "", fmt.Errorf("%s has no .claude.json yet — run `CLAUDE_CONFIG_DIR=%s claude` once to set it up", dir, dir)
	}
	return dir, nil
}

// LiveSlot returns the managed slot the current configuration directory is
// logged in as, or 0. Under a terminal-scoped switch that is this
// terminal's seat, which state.ActiveSlot — the shared login's seat — is
// not.
func LiveSlot(state *store.State) int {
	_, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil {
		return 0
	}
	return state.FindByIdentity(parsed.EmailAddress, parsed.OrganizationUUID)
}

// SwitchTerminal points this process, and every child it starts afterwards,
// at the target seat's own profile directory. It never writes the shared
// login, the shared .claude.json, or state.json; it writes the seat's
// stored login into the profile only when the profile is not already logged
// in as that seat.
//
// On success CLAUDE_CONFIG_DIR and CUX_SCOPE are set in this process's
// environment. On failure they are left as they were.
func SwitchTerminal(identifier string) (from, to store.Account, err error) {
	if err := ensureBackupRoot(); err != nil {
		return store.Account{}, store.Account{}, err
	}
	lk, err := lockfile.Acquire(paths.LockFile(), lockTimeout)
	if err != nil {
		return store.Account{}, store.Account{}, err
	}
	defer lk.Unlock()

	state, err := store.Load()
	if err != nil {
		return store.Account{}, store.Account{}, err
	}
	if len(state.Accounts) == 0 {
		return store.Account{}, store.Account{}, store.ErrEmptyState
	}
	target, err := state.Resolve(identifier)
	if err != nil {
		return store.Account{}, store.Account{}, err
	}
	callerSlot := LiveSlot(state)
	if callerSlot != 0 {
		from = state.Accounts[callerSlot]
	}
	dir, err := ProfileDir(target)
	if err != nil {
		return store.Account{}, store.Account{}, err
	}

	prev := map[string]*string{}
	for _, k := range []string{paths.EnvClaudeConfigDir, EnvScope} {
		if v, ok := os.LookupEnv(k); ok {
			prev[k] = &v
		} else {
			prev[k] = nil
		}
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		for k, v := range prev {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	}()
	// creds and claudecfg resolve every path from CLAUDE_CONFIG_DIR, so
	// pointing it at the profile first makes the reads and writes below
	// land in the profile through the same code a shared swap uses.
	if err := os.Setenv(paths.EnvClaudeConfigDir, dir); err != nil {
		return store.Account{}, store.Account{}, err
	}
	if err := os.Setenv(EnvScope, ScopeTerminal); err != nil {
		return store.Account{}, store.Account{}, err
	}

	// A profile the seat's own claude already logged into keeps refreshing
	// its token there. Overwriting it with the stored copy would put an old
	// refresh token back and sign that profile out.
	if _, liveErr := creds.ReadLive(); liveErr == nil && LiveSlot(state) == target.Slot {
		committed = true
		return from, target, nil
	}

	// Copying the login this terminal is running on would leave one refresh
	// token in two places, and whichever refreshes first revokes the other.
	if callerSlot == target.Slot {
		return store.Account{}, store.Account{}, fmt.Errorf(
			"slot %d (%s) is the login this terminal already uses and %s is not logged in as it; "+
				"copying it would share one refresh token between two logins — run `CLAUDE_CONFIG_DIR=%s claude` and /login once instead",
			target.Slot, target.Email, dir, dir)
	}

	targetCreds, err := creds.ReadBackup(target.Slot, target.Email)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("target credentials missing: %w", err)
	}
	if _, tokErr := creds.ExtractAccessToken(targetCreds); tokErr != nil {
		return store.Account{}, store.Account{}, fmt.Errorf(
			"slot %d (%s) has no usable stored login — run `claude login` as %s, then `cux add` to recapture the slot",
			target.Slot, target.Email, target.Email)
	}
	targetOAuth, err := store.ReadOAuthBlockBackup(target.Slot, target.Email)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("target oauthAccount missing: %w", err)
	}

	rollbackCreds, _ := creds.ReadLive()
	rollbackOAuth, _, _ := claudecfg.ReadOAuthBlock()
	if err := creds.WriteLive(targetCreds); err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("writing %s credentials: %w", dir, err)
	}
	if err := claudecfg.WriteOAuthBlock(targetOAuth); err != nil {
		if rollbackCreds != "" {
			_ = creds.WriteLive(rollbackCreds)
		}
		if len(rollbackOAuth) > 0 {
			_ = claudecfg.WriteOAuthBlock(rollbackOAuth)
		}
		return store.Account{}, store.Account{}, fmt.Errorf("writing %s oauthAccount: %w", dir, err)
	}
	committed = true
	return from, target, nil
}
