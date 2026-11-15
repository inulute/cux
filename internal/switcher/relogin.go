package switcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// LoginFunc runs Claude Code's interactive login with its config directory
// pointed at dir, pre-filling email. It returns once the login has finished
// or failed. The production implementation runs `claude auth login`; tests
// substitute one that writes a login into dir.
type LoginFunc func(dir, email string) error

// ReloginResult describes a completed Relogin.
type ReloginResult struct {
	Account store.Account
	// Live is set when the slot was the live login, which Relogin then
	// replaced as well, so the sessions on it pick up the new login.
	Live bool
}

// reloginLockTimeout bounds the wait for the cux lock after the login.
const reloginLockTimeout = time.Minute

// ErrReloginWrongAccount is returned when the login that finished is not
// the account the slot belongs to — another email, or the same email in
// another organization. Nothing has been written when it is returned.
var ErrReloginWrongAccount = errors.New("switcher: the login is for a different account than the slot")

// Relogin logs one managed slot in again without disturbing anything else.
//
// The login runs in a fresh private directory under cux's runtime dir, so
// Claude Code writes the new credentials and identity there rather than over
// the live login that running sessions use (and keep refreshing). Once it
// returns, the identity it wrote is compared with the slot's — email,
// organization and account UUID — and only a match is imported, into that
// slot's backups alone. The active slot is never changed. If the slot is the
// live login, the live credentials are replaced too, since those are the
// ones that failed.
//
// The usage reading cached for the slot is dropped, so `cux list` stops
// showing the failure recorded against the old login. The staging directory
// (and on macOS its Keychain item) is removed whether or not the import
// succeeds: it holds a working login, and nothing reads it afterwards.
//
// The cux lock is held only for the import, never during the interactive
// login, which can take minutes while other sessions need the lock.
func Relogin(identifier string, login LoginFunc, out io.Writer) (ReloginResult, error) {
	if err := ensureBackupRoot(); err != nil {
		return ReloginResult{}, err
	}
	state, err := store.Load()
	if err != nil {
		return ReloginResult{}, err
	}
	if len(state.Accounts) == 0 {
		return ReloginResult{}, store.ErrEmptyState
	}
	target, err := state.Resolve(identifier)
	if err != nil {
		return ReloginResult{}, err
	}

	stage, err := os.MkdirTemp(paths.RuntimeDir(), "login-")
	if err != nil {
		return ReloginResult{}, fmt.Errorf("switcher: create login directory: %w", err)
	}
	defer func() {
		if err := creds.DeleteLoginAt(stage); err != nil {
			fmt.Fprintf(out, "warning: could not remove the temporary login from the keychain: %v\n", err)
		}
		if err := os.RemoveAll(stage); err != nil {
			fmt.Fprintf(out, "warning: could not remove %s: %v\n", stage, err)
		}
	}()

	org := orgNameOf(target)
	fmt.Fprintf(out, "Logging in slot %d (%s).\n", target.Slot, describeSeat(target, org))
	fmt.Fprintf(out, "Sign in as %s in the browser", target.Email)
	if org != "" {
		fmt.Fprintf(out, " and pick the organization %q", org)
	}
	fmt.Fprintln(out, ". If asked for a code, paste it and press Enter; it may not be shown as you paste.")

	loginErr := login(stage, target.Email)
	if loginErr != nil {
		fmt.Fprintln(out, "Login did not complete; nothing was changed.")
		return ReloginResult{}, fmt.Errorf("switcher: login: %w", loginErr)
	}
	fmt.Fprintln(out, "Login finished. Checking which account signed in...")

	blob, err := creds.ReadLoginAt(stage)
	if err != nil {
		return ReloginResult{}, fmt.Errorf("switcher: the login stored no usable credentials: %w", err)
	}
	rawOAuth, parsed, err := claudecfg.ReadOAuthBlockAt(filepath.Join(stage, ".claude.json"))
	if err != nil {
		return ReloginResult{}, fmt.Errorf("switcher: the login stored no account identity: %w", err)
	}
	if err := sameSeat(target, org, parsed); err != nil {
		return ReloginResult{}, err
	}

	// The login is done and would be lost to a timeout, so wait longer
	// for the lock than the usual short operations do.
	lk, err := lockfile.Acquire(paths.LockFile(), reloginLockTimeout)
	if err != nil {
		fmt.Fprintln(out, "Could not take the cux lock; the new login was discarded. Run the command again.")
		return ReloginResult{}, err
	}
	defer lk.Unlock()

	// Re-read under the lock: the slot may have been removed or reused
	// while the login was in the browser.
	state, err = store.Load()
	if err != nil {
		return ReloginResult{}, err
	}
	current, ok := state.Accounts[target.Slot]
	if !ok {
		return ReloginResult{}, fmt.Errorf("switcher: slot %d was removed while logging in; nothing was changed", target.Slot)
	}
	if err := sameSeat(current, org, parsed); err != nil {
		return ReloginResult{}, err
	}
	if slot, other := slotSharingToken(state, current.Slot, blob); slot != 0 {
		return ReloginResult{}, fmt.Errorf("%w: the new login carries the token already stored for slot %d (%s); nothing was changed",
			ErrTokenIdentityMismatch, slot, other)
	}

	// Is the slot the live login? Decided by identity, never by comparing
	// the live files' bytes: a running session rewrites those whenever it
	// refreshes its token.
	isLive := isLiveSeat(state, current)

	if err := creds.WriteBackup(current.Slot, current.Email, blob); err != nil {
		return ReloginResult{}, err
	}
	if err := store.WriteOAuthBlockBackup(current.Slot, current.Email, rawOAuth); err != nil {
		return ReloginResult{}, err
	}
	if isLive {
		if err := creds.WriteLive(blob); err != nil {
			return ReloginResult{}, fmt.Errorf("slot %d is saved, but writing the live login failed: %w", current.Slot, err)
		}
		if err := claudecfg.WriteOAuthBlock(rawOAuth); err != nil {
			return ReloginResult{}, fmt.Errorf("slot %d is saved, but writing the live account block failed: %w", current.Slot, err)
		}
	}

	// The cached reading describes the old login's failure (an expired
	// token, a poll cooldown), and would go on being shown until it aged
	// out. The cache is written under this same lock by every poller.
	if err := dropUsageReading(current); err != nil {
		fmt.Fprintf(out, "warning: could not clear the cached usage for slot %d: %v\n", current.Slot, err)
	}
	return ReloginResult{Account: current, Live: isLive}, nil
}

// isLiveSeat reports whether acct is the seat the live login belongs to,
// resolved the way the hooks and the wrapper resolve it (store.LiveSeat),
// plus the account UUID when both sides have one. Anything it cannot
// establish counts as not live, so the live login is left alone.
func isLiveSeat(state *store.State, acct store.Account) bool {
	_, live, err := claudecfg.ReadOAuthBlock()
	if err != nil || live.EmailAddress == "" {
		return false
	}
	liveKey := store.Account{Email: live.EmailAddress, UUID: live.AccountUUID, OrgUUID: live.OrganizationUUID}.CacheKey()
	seat, ok := state.LiveSeat(live.EmailAddress, liveKey)
	if !ok || seat.Slot != acct.Slot {
		return false
	}
	if acct.UUID != "" && live.AccountUUID != "" && acct.UUID != live.AccountUUID {
		return false
	}
	return true
}

// sameSeat reports whether a login's identity is the seat acct belongs to.
// The email alone is not enough: one email can hold seats in several
// organizations, and a login in the wrong one would file that seat's token
// under this slot. A legacy slot with no stored organization or account
// UUID can only be checked on what it has.
func sameSeat(acct store.Account, org string, got claudecfg.OAuthAccount) error {
	if got.EmailAddress == acct.Email &&
		(acct.OrgUUID == "" || got.OrganizationUUID == acct.OrgUUID) &&
		(acct.UUID == "" || got.AccountUUID == acct.UUID) {
		return nil
	}
	gotDesc := got.EmailAddress
	if gotDesc == "" {
		gotDesc = "(no email)"
	}
	if got.OrganizationName != "" {
		gotDesc += fmt.Sprintf(" in %q", got.OrganizationName)
	} else if got.OrganizationUUID != "" {
		gotDesc += " in organization " + got.OrganizationUUID
	}
	return fmt.Errorf("%w: signed in as %s, but slot %d is %s; nothing was changed",
		ErrReloginWrongAccount, gotDesc, acct.Slot, describeSeat(acct, org))
}

// describeSeat names a seat for messages: its email, its organization
// (org, else the stored UUID) and its alias.
func describeSeat(acct store.Account, org string) string {
	s := acct.Email
	if org != "" {
		s += fmt.Sprintf(" in %q", org)
	} else if acct.OrgUUID != "" {
		s += " in organization " + acct.OrgUUID
	}
	if acct.Alias != "" {
		s += " · " + acct.Alias
	}
	return s
}

// orgNameOf reads the organization name from the slot's saved oauthAccount
// block. The state file does not keep it; the block does. "" when unknown.
func orgNameOf(acct store.Account) string {
	raw, err := store.ReadOAuthBlockBackup(acct.Slot, acct.Email)
	if err != nil {
		return ""
	}
	var parsed claudecfg.OAuthAccount
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	return parsed.OrganizationName
}

// dropUsageReading removes acct's entry from the usage cache. The caller
// holds the cux lock.
func dropUsageReading(acct store.Account) error {
	cache, err := usage.LoadCache()
	if err != nil {
		return err
	}
	if _, ok := cache[acct.CacheKey()]; !ok {
		return nil
	}
	delete(cache, acct.CacheKey())
	return usage.SaveCache(cache)
}
