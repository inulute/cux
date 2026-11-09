package switcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/profile"
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
	ScopeTerminal = profile.ScopeTerminal
	ScopeAll      = "all"

	// EnvScope marks a process that runs on a seat's own profile because of
	// a terminal-scoped switch. Anything that would swap the shared login
	// from inside it moves this process instead, or refuses.
	EnvScope = profile.EnvScope
)

// TerminalScoped reports whether this process runs on a seat's own profile
// through a terminal-scoped switch.
func TerminalScoped() bool { return profile.TerminalScoped() }

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
// $HOME/.claude-<alias>. It must already exist, hold a .claude.json and
// carry cux's hooks and /switch: pointing claude at an empty directory
// would start it with no settings, plugins or hooks, and cux never creates
// Claude Code's config from scratch (see claudecfg.WriteOAuthBlock).
func ProfileDir(a store.Account) (string, error) { return profile.Check(a) }

// PrepareTerminal checks, without writing anything, that this terminal can
// move to target's profile: the profile is ready, and target's login can
// get there without a second live holder. A /switch runs it before the
// wrapper stops claude, so a refusal does not cost the session.
func PrepareTerminal(state *store.State, target store.Account) (dir string, needsCopy bool, err error) {
	dir, err = profile.Check(target)
	if err != nil {
		return "", false, err
	}
	needsCopy, err = profile.NeedsCopy(state, target, dir)
	if err != nil {
		return "", false, err
	}
	return dir, needsCopy, nil
}

// SessionSlot is the seat this process runs on: its own profile's seat
// under a terminal scope, the shared login's seat (state.ActiveSlot)
// otherwise.
func SessionSlot(state *store.State) int {
	if TerminalScoped() {
		return LiveSlot(state)
	}
	return state.ActiveSlot
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
// login, the shared .claude.json, or state.json. It copies the seat's
// stored login into the profile only when the profile is not logged in as
// that seat and no other directory is either (see profile.NeedsCopy).
//
// On success CLAUDE_CONFIG_DIR and CUX_SCOPE are set in this process's
// environment, and CUX_SHARED_CONFIG_DIR records the directory the first
// scoped switch left. On failure all three are left as they were.
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
	dir, needsCopy, err := PrepareTerminal(state, target)
	if err != nil {
		return store.Account{}, store.Account{}, err
	}

	var targetCreds string
	var targetOAuth json.RawMessage
	if needsCopy {
		targetCreds, err = creds.ReadBackup(target.Slot, target.Email)
		if err != nil {
			return store.Account{}, store.Account{}, fmt.Errorf("target credentials missing: %w", err)
		}
		if _, tokErr := creds.ExtractAccessToken(targetCreds); tokErr != nil {
			return store.Account{}, store.Account{}, fmt.Errorf(
				"slot %d (%s) has no usable stored login — run `claude login` as %s, then `cux add` to recapture the slot",
				target.Slot, target.Email, target.Email)
		}
		targetOAuth, err = store.ReadOAuthBlockBackup(target.Slot, target.Email)
		if err != nil {
			return store.Account{}, store.Account{}, fmt.Errorf("target oauthAccount missing: %w", err)
		}
	}

	// Leaving a seat's own profile: if its claude refreshed the login since
	// cux last looked, the backup now holds a revoked token. Take the
	// profile's, the way a shared swap saves the seat it leaves.
	if callerSlot != 0 && callerSlot != target.Slot {
		if _, err := profile.Reconcile(state, from); err != nil {
			return store.Account{}, store.Account{}, fmt.Errorf("saving %s's login before leaving it: %w", from.Email, err)
		}
	}

	if needsCopy {
		undo, err := creds.CopyAccountLoginIn(dir, targetCreds)
		if err != nil {
			return store.Account{}, store.Account{}, fmt.Errorf("writing %s credentials: %w", dir, err)
		}
		if err := claudecfg.WriteOAuthBlockIn(dir, targetOAuth); err != nil {
			// The profile was not logged in as this seat before; leaving
			// the copied login there would give it a holder nothing tracks.
			if undoErr := undo(); undoErr != nil {
				return store.Account{}, store.Account{}, fmt.Errorf(
					"writing %s oauthAccount: %w (and removing the copied login failed: %v)", dir, err, undoErr)
			}
			return store.Account{}, store.Account{}, fmt.Errorf("writing %s oauthAccount: %w", dir, err)
		}
	}

	if !TerminalScoped() {
		if err := os.Setenv(profile.EnvSharedConfigDir, os.Getenv(paths.EnvClaudeConfigDir)); err != nil {
			return store.Account{}, store.Account{}, err
		}
	}
	if err := os.Setenv(paths.EnvClaudeConfigDir, dir); err != nil {
		return store.Account{}, store.Account{}, err
	}
	if err := os.Setenv(EnvScope, ScopeTerminal); err != nil {
		return store.Account{}, store.Account{}, err
	}
	return from, target, nil
}
