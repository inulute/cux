// Package profile knows where a seat's login can be live.
//
// Claude Code keeps a login per configuration directory: the shared one
// ($HOME/.claude, or $CLAUDE_CONFIG_DIR) and a seat's own profile,
// $HOME/.claude-<alias>, that a terminal-scoped switch points a terminal
// at. A refresh token must be live in one of them at most. The OAuth server
// rotates the refresh token on every refresh and revokes the old one, so
// two directories holding one token sign each other out: whichever
// refreshes first revokes the other.
//
// cux's own backup of a seat is the one copy allowed beside the live one,
// and only because cux never refreshes it while another directory runs on
// the same token: the monitor refreshes a backup only when no directory but
// its own process's, which it can hand the new token to, holds that token.
// This package is where that
// is decided: which directories hold a seat, whether a stored login may be
// copied into a profile, and how a profile's login and the backup follow
// each other.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

const (
	// EnvScope marks a process that runs on a seat's own profile because of
	// a terminal-scoped switch.
	EnvScope = "CUX_SCOPE"
	// ScopeTerminal is EnvScope's value in such a process.
	ScopeTerminal = "terminal"
	// EnvSharedConfigDir records the CLAUDE_CONFIG_DIR a terminal had before
	// a terminal-scoped switch pointed it at a profile ("" for the default),
	// so the shared login can still be found from inside the profile.
	EnvSharedConfigDir = "CUX_SHARED_CONFIG_DIR"
)

// TerminalScoped reports whether this process runs on a seat's own profile
// through a terminal-scoped switch.
func TerminalScoped() bool {
	return os.Getenv(EnvScope) == ScopeTerminal
}

// Dir is a seat's own profile directory, $HOME/.claude-<alias>.
func Dir(alias string) string {
	return filepath.Join(paths.Home(), ".claude-"+alias)
}

// Label names a configuration directory for messages.
func Label(configDir string) string {
	if configDir == "" {
		return "the default " + paths.ClaudeDirIn("")
	}
	return configDir
}

// Check returns the seat's profile directory once it is ready to run a
// claude on: it exists, Claude Code initialised it (.claude.json), and cux
// setup ran there. A claude started on a profile reads its hooks and slash
// commands from that profile only; without cux's, the session would lose
// /switch and the automatic switching with nothing to say so.
func Check(a store.Account) (string, error) {
	if a.Alias == "" {
		return "", fmt.Errorf("slot %d (%s) has no alias, so it has no profile directory — "+
			"set one with `cux alias %d <name>`, then create ~/.claude-<name>", a.Slot, a.Email, a.Slot)
	}
	dir := Dir(a.Alias)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("profile directory %s for slot %d (%s) does not exist — "+
			"create it with `CLAUDE_CONFIG_DIR=%s claude` once", dir, a.Slot, a.Email, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil {
		return "", fmt.Errorf("%s has no .claude.json yet — run `CLAUDE_CONFIG_DIR=%s claude` once to set it up", dir, dir)
	}
	hooks, err := hookinstall.InstalledIn(dir)
	if err != nil {
		return "", fmt.Errorf("reading %s's cux hooks: %w", dir, err)
	}
	_, cmdErr := os.Stat(filepath.Join(dir, "commands", "switch.md"))
	if !hooks || cmdErr != nil {
		return "", fmt.Errorf("%s has no cux hooks or /switch, so a session moved there could not switch again — "+
			"run `CLAUDE_CONFIG_DIR=%s cux setup` once", dir, dir)
	}
	return dir, nil
}

// ConfigDirs returns every Claude Code configuration directory a login can
// be live in on this machine: the default one, $HOME/.claude named
// explicitly, the shared directory a scoped terminal came from, this
// process's own, and every initialised $HOME/.claude-* directory. Two
// directories that store their login in one place count once.
func ConfigDirs() []string {
	cands := []string{
		"",
		paths.ClaudeDirIn(""),
		os.Getenv(EnvSharedConfigDir),
		os.Getenv(paths.EnvClaudeConfigDir),
	}
	matches, _ := filepath.Glob(filepath.Join(paths.Home(), ".claude-*"))
	sort.Strings(matches)
	for _, m := range matches {
		if fi, err := os.Stat(filepath.Join(m, ".claude.json")); err == nil && !fi.IsDir() {
			cands = append(cands, m)
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for i, d := range cands {
		if d == "" && i > 0 {
			continue
		}
		loc := creds.LiveLocation(d)
		if seen[loc] {
			continue
		}
		seen[loc] = true
		out = append(out, d)
	}
	return out
}

// Holder is a configuration directory logged in as a seat.
type Holder struct {
	Dir  string
	Blob string
}

// Holders returns the directories logged in as slot with a login that can
// refresh. A login without a refresh token cannot rotate, so it cannot
// revoke anyone and is left out.
func Holders(state *store.State, slot int) ([]Holder, error) {
	var out []Holder
	for _, d := range ConfigDirs() {
		blob, ok, err := loginIn(state, slot, d)
		if err != nil {
			return nil, err
		}
		if ok && creds.HasRefreshToken(blob) {
			out = append(out, Holder{Dir: d, Blob: blob})
		}
	}
	return out, nil
}

// loginIn returns configDir's live login when that directory says it is
// logged in as slot.
func loginIn(state *store.State, slot int, configDir string) (string, bool, error) {
	_, id, err := claudecfg.ReadOAuthBlockIn(configDir)
	if err != nil || state.FindByIdentity(id.EmailAddress, id.OrganizationUUID) != slot {
		return "", false, nil
	}
	blob, err := creds.ReadLiveIn(configDir)
	switch {
	case err == nil:
		return blob, true, nil
	case errors.Is(err, creds.ErrNotFound), errors.Is(err, creds.ErrNoAccountToken):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("reading the login in %s: %w", Label(configDir), err)
	}
}

// NeedsCopy reports whether target's stored login has to be copied into
// dir, its profile, and refuses when that copy would give the login a
// second live holder. It writes nothing.
func NeedsCopy(state *store.State, target store.Account, dir string) (bool, error) {
	if _, ok, err := loginIn(state, target.Slot, dir); err != nil {
		return false, err
	} else if ok {
		// The profile's own claude keeps this login fresh; nothing to copy.
		return false, nil
	}
	holders, err := Holders(state, target.Slot)
	if err != nil {
		return false, err
	}
	for _, h := range holders {
		if creds.LiveLocation(h.Dir) != creds.LiveLocation(dir) {
			return false, fmt.Errorf(
				"slot %d (%s) is logged in at %s and %s is not; copying cux's stored login into %s would leave "+
					"one refresh token in two places, and whichever refreshes first signs the other out — "+
					"run `CLAUDE_CONFIG_DIR=%s claude` and /login once instead",
				target.Slot, target.Email, Label(h.Dir), dir, dir, dir)
		}
	}
	return true, nil
}

// LiveElsewhere returns a directory other than configDir that holds slot's
// login, or "" and false. A copy of slot's stored login into configDir
// would give that login a second holder.
func LiveElsewhere(state *store.State, slot int, configDir string) (string, bool, error) {
	holders, err := Holders(state, slot)
	if err != nil {
		return "", false, err
	}
	for _, h := range holders {
		if creds.LiveLocation(h.Dir) != creds.LiveLocation(configDir) {
			return h.Dir, true, nil
		}
	}
	return "", false, nil
}

// HeldElsewhere returns a directory other than configDir that is logged in
// with stored's login, the same refresh token. That directory's
// claude runs on the token, so refreshing stored anywhere else would revoke
// it there, and only configDir's own login can be handed the new one. It
// checks by token, not by identity file: a directory logged in as the seat
// on its own, with another token, is not affected by a refresh of stored.
func HeldElsewhere(state *store.State, slot int, configDir, stored string) (Holder, bool, error) {
	holders, err := Holders(state, slot)
	if err != nil {
		return Holder{}, false, err
	}
	for _, h := range holders {
		if creds.LiveLocation(h.Dir) != creds.LiveLocation(configDir) && creds.SameLogin(h.Blob, stored) {
			return h, true, nil
		}
	}
	return Holder{}, false, nil
}

// Reconcile brings a seat's backup up to date with its own profile when the
// profile's claude refreshed the login since cux last saw it. Until then
// the backup holds a refresh token the server already revoked, and any
// later copy or poll from it would sign the profile out.
//
// It adopts the profile's login only when the profile is the seat's sole
// holder. A seat also live somewhere else was logged in there separately;
// the backup follows that one, and the profile's own claude keeps the
// profile's login.
//
// owned reports whether the profile is logged in as the seat with a login
// that can refresh, so a caller polling the seat need not read it again.
func Reconcile(state *store.State, a store.Account) (owned bool, err error) {
	if a.Alias == "" {
		return false, nil
	}
	dir := Dir(a.Alias)
	live, ok, err := loginIn(state, a.Slot, dir)
	if err != nil || !ok || !creds.HasRefreshToken(live) {
		return false, err
	}
	backup, err := creds.ReadBackup(a.Slot, a.Email)
	if err != nil {
		if errors.Is(err, creds.ErrNotFound) {
			return true, nil
		}
		return true, err
	}
	if creds.SameLogin(live, backup) || creds.ExpiresAt(live) <= creds.ExpiresAt(backup) {
		return true, nil
	}
	if _, elsewhere, err := LiveElsewhere(state, a.Slot, dir); err != nil || elsewhere {
		return true, err
	}
	merged, err := creds.WithAccountLogin(backup, live)
	if err != nil {
		return true, err
	}
	return true, creds.WriteBackup(a.Slot, a.Email, merged)
}

// SyncRefreshed hands a token cux just refreshed from the backup to the
// seat's own profile, when the profile still holds the token that refresh
// revoked. A profile holding another login of the seat is left alone: it
// was logged in on its own and its claude refreshes it.
func SyncRefreshed(state *store.State, a store.Account, old, fresh string) error {
	if a.Alias == "" {
		return nil
	}
	dir := Dir(a.Alias)
	live, ok, err := loginIn(state, a.Slot, dir)
	if err != nil || !ok {
		return err
	}
	if creds.SameLogin(live, fresh) || !creds.SameLogin(live, old) {
		return nil
	}
	_, err = creds.CopyAccountLoginIn(dir, fresh)
	return err
}
