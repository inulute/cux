// Package switcher orchestrates the high-level operations on top of
// store, creds, and claudecfg: adding the currently-logged-in account,
// switching the active account, removing one. Each operation runs under
// the on-disk lock so two terminals cannot corrupt state.json by racing.
package switcher

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

const lockTimeout = 10 * time.Second

// AddCurrent reads the currently-logged-in account from Claude Code's
// live storage and registers it in cux. If the account is already
// managed, its credential and oauth backups are refreshed in place
// rather than rejected — this is the natural way to refresh a stale
// token (`claude login` again, then `cux add`).
//
// alias is optional: pass "" to auto-derive from the account's displayName.
// Pass skipAutoAlias=true to store the account with no alias at all.
// When alias is non-empty it must pass store.ValidateAlias and be unique.
func AddCurrent(preferredSlot int, alias string, skipAutoAlias bool) (added store.Account, refreshed bool, err error) {
	if err := ensureBackupRoot(); err != nil {
		return store.Account{}, false, err
	}
	lk, err := lockfile.Acquire(paths.LockFile(), lockTimeout)
	if err != nil {
		return store.Account{}, false, err
	}
	defer lk.Unlock()

	state, err := store.Load()
	if err != nil {
		return store.Account{}, false, err
	}

	liveCreds, err := creds.ReadLive()
	if err != nil {
		if errors.Is(err, creds.ErrNotFound) {
			return store.Account{}, false, errors.New("no active Claude Code login found — run `claude login` first")
		}
		return store.Account{}, false, err
	}

	rawOAuth, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil {
		return store.Account{}, false, err
	}
	if parsed.EmailAddress == "" {
		return store.Account{}, false, errors.New("oauthAccount block has no emailAddress")
	}
	if err := store.ValidateEmail(parsed.EmailAddress); err != nil {
		return store.Account{}, false, err
	}

	// Validate alias early so we fail before touching any files.
	// When no alias is explicitly provided, auto-derive one from the
	// account's displayName so users get friendly names without extra steps.
	if alias != "" {
		if err := store.ValidateAlias(alias); err != nil {
			return store.Account{}, false, err
		}
	} else if !skipAutoAlias && parsed.DisplayName != "" {
		alias = SuggestAlias(state, parsed.DisplayName, parsed.OrganizationName)
	}

	if existing := state.FindByIdentity(parsed.EmailAddress, parsed.OrganizationUUID); existing != 0 {
		// Refresh: overwrite backups for this slot, no state shape change.
		acct := state.Accounts[existing]
		if err := creds.WriteBackup(existing, acct.Email, liveCreds); err != nil {
			return store.Account{}, false, err
		}
		if err := store.WriteOAuthBlockBackup(existing, acct.Email, rawOAuth); err != nil {
			return store.Account{}, false, err
		}
		acct.LastUsed = time.Now().UTC()
		// Update alias if provided (allows renaming on re-add).
		if alias != "" {
			if err := state.SetAlias(existing, alias); err != nil {
				return store.Account{}, false, err
			}
			acct = state.Accounts[existing]
		}
		state.Accounts[existing] = acct
		state.ActiveSlot = existing
		if err := state.Save(); err != nil {
			return store.Account{}, false, err
		}
		return acct, true, nil
	}

	slot := preferredSlot
	if slot <= 0 {
		slot = state.NextSlot()
	} else if _, taken := state.Accounts[slot]; taken {
		return store.Account{}, false, fmt.Errorf("slot %d already in use", slot)
	}

	if alias != "" {
		if existing := state.FindByAlias(alias); existing != 0 {
			return store.Account{}, false, fmt.Errorf("store: alias %q is already used by slot %d (%s)", alias, existing, state.Accounts[existing].Email)
		}
	}

	if err := creds.WriteBackup(slot, parsed.EmailAddress, liveCreds); err != nil {
		return store.Account{}, false, err
	}
	if err := store.WriteOAuthBlockBackup(slot, parsed.EmailAddress, rawOAuth); err != nil {
		// Roll back the credential backup so we don't leave half an account on disk.
		_ = creds.DeleteBackup(slot, parsed.EmailAddress)
		return store.Account{}, false, err
	}
	if err := state.Add(slot, parsed.EmailAddress, parsed.AccountUUID, parsed.OrganizationUUID); err != nil {
		_ = creds.DeleteBackup(slot, parsed.EmailAddress)
		_ = store.DeleteOAuthBlockBackup(slot, parsed.EmailAddress)
		return store.Account{}, false, err
	}
	if alias != "" {
		if err := state.SetAlias(slot, alias); err != nil {
			_ = creds.DeleteBackup(slot, parsed.EmailAddress)
			_ = store.DeleteOAuthBlockBackup(slot, parsed.EmailAddress)
			_ = state.Remove(slot)
			return store.Account{}, false, err
		}
	}
	state.ActiveSlot = slot
	if err := state.Save(); err != nil {
		return store.Account{}, false, err
	}
	return state.Accounts[slot], false, nil
}

// SwitchTo activates the target account: backs up the current account's
// (possibly refreshed) credentials, then writes the target's credentials
// and oauthAccount block to Claude Code's live storage.
//
// The operation is staged: target backups are read and validated before
// any live state is touched, so a missing or corrupt backup aborts
// without disturbing the running login.
func SwitchTo(identifier string) (from, to store.Account, err error) {
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

	// Stage: read target backups before touching anything live.
	targetCreds, err := creds.ReadBackup(target.Slot, target.Email)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("target credentials missing: %w", err)
	}
	targetOAuth, err := store.ReadOAuthBlockBackup(target.Slot, target.Email)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("target oauthAccount missing: %w", err)
	}

	// Refresh-backup of current live state. We always do this — the
	// access token may have rotated since the last add, and we don't
	// want to overwrite a fresher copy with a stale one.
	currentLive, liveErr := creds.ReadLive()
	currentRaw, currentParsed, cfgErr := claudecfg.ReadOAuthBlock()
	var current store.Account
	if liveErr == nil && cfgErr == nil {
		if slot := state.FindByIdentity(currentParsed.EmailAddress, currentParsed.OrganizationUUID); slot != 0 {
			current = state.Accounts[slot]
			if err := creds.WriteBackup(slot, current.Email, currentLive); err != nil {
				return store.Account{}, store.Account{}, fmt.Errorf("backing up current creds: %w", err)
			}
			if err := store.WriteOAuthBlockBackup(slot, current.Email, currentRaw); err != nil {
				return store.Account{}, store.Account{}, fmt.Errorf("backing up current oauth: %w", err)
			}
		}
		// If the live account isn't managed, we silently proceed — we
		// just won't have a backup for it. Better than failing to switch.
	}

	// Snapshot live state so we can restore on failure mid-write.
	rollbackCreds := currentLive
	rollbackOAuth := currentRaw

	if err := creds.WriteLive(targetCreds); err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("writing live credentials: %w", err)
	}
	if err := claudecfg.WriteOAuthBlock(targetOAuth); err != nil {
		// Best-effort rollback of the credential write.
		if rollbackCreds != "" {
			_ = creds.WriteLive(rollbackCreds)
		}
		if len(rollbackOAuth) > 0 {
			_ = claudecfg.WriteOAuthBlock(rollbackOAuth)
		}
		return store.Account{}, store.Account{}, fmt.Errorf("writing oauthAccount: %w", err)
	}

	target.LastUsed = time.Now().UTC()
	state.Accounts[target.Slot] = target
	state.ActiveSlot = target.Slot
	if err := state.Save(); err != nil {
		// State save failed but the live swap succeeded — surface this
		// loudly. The next cux run will re-derive active from .claude.json.
		return current, target, fmt.Errorf("swap complete but state save failed: %w", err)
	}
	return current, target, nil
}

// Remove unregisters an account and deletes its credential + oauth backups.
// Refuses to remove the currently-active account unless force is set.
func Remove(identifier string, force bool) (store.Account, error) {
	lk, err := lockfile.Acquire(paths.LockFile(), lockTimeout)
	if err != nil {
		return store.Account{}, err
	}
	defer lk.Unlock()

	state, err := store.Load()
	if err != nil {
		return store.Account{}, err
	}
	target, err := state.Resolve(identifier)
	if err != nil {
		return store.Account{}, err
	}
	if state.ActiveSlot == target.Slot && !force {
		return store.Account{}, fmt.Errorf("slot %d (%s) is currently active — pass --force to remove anyway", target.Slot, target.Email)
	}

	if err := creds.DeleteBackup(target.Slot, target.Email); err != nil {
		return store.Account{}, err
	}
	if err := store.DeleteOAuthBlockBackup(target.Slot, target.Email); err != nil {
		return store.Account{}, err
	}
	if err := state.Remove(target.Slot); err != nil {
		return store.Account{}, err
	}
	if err := state.Save(); err != nil {
		return store.Account{}, err
	}
	return target, nil
}

// CurrentLiveEmail returns the email of whichever account is currently
// logged in to Claude Code, regardless of whether it's managed by cux.
func CurrentLiveEmail() (string, error) {
	_, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil {
		return "", err
	}
	return parsed.EmailAddress, nil
}

// CurrentLiveCacheKey returns the usage-cache key for the currently active
// account. When the account has an organizationUuid that is used; otherwise
// the email is returned for backward compatibility.
func CurrentLiveCacheKey() (string, error) {
	_, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil {
		return "", err
	}
	acct := store.Account{Email: parsed.EmailAddress, OrgUUID: parsed.OrganizationUUID}
	return acct.CacheKey(), nil
}

func ensureBackupRoot() error {
	if err := osMkdirAll(paths.BackupRoot()); err != nil {
		return err
	}
	if err := osMkdirAll(paths.AccountsDir()); err != nil {
		return err
	}
	if err := osMkdirAll(paths.RuntimeDir()); err != nil {
		return err
	}
	return nil
}

// --- alias suggestion -------------------------------------------------------

// nonAlphanumRE matches anything that isn't a lowercase letter or digit.
var nonAlphanumRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugify converts a human display name into a valid alias candidate:
//   - lower-case
//   - spaces become hyphens
//   - non-alphanumeric characters (except hyphens) are removed
//   - leading digits get an "a" prefix (alias must start with a letter)
//   - truncated to 20 characters
//
// Returns "" when the result is empty (e.g. the name was all symbols).
func slugify(name string) string {
	// Normalise: fold to ASCII lower-case, map spaces to hyphens.
	var sb strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsSpace(r) {
			if !prevHyphen {
				sb.WriteRune('-')
				prevHyphen = true
			}
		} else if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			sb.WriteRune(r)
			prevHyphen = (r == '-')
		}
		// everything else is dropped
	}
	s := strings.Trim(sb.String(), "-")
	// Collapse consecutive hyphens.
	s = nonAlphanumRE.ReplaceAllLiteralString(s, "-")
	// Aliases must start with a letter.
	if len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		s = "a" + s
	}
	// Truncate.
	runes := []rune(s)
	if len(runes) > 20 {
		s = string(runes[:20])
	}
	s = strings.TrimRight(s, "-")
	return s
}

// SuggestAlias returns a unique alias for an account being added, derived
// from its displayName and (if needed for disambiguation) its
// organizationName. The returned alias is always valid per store.ValidateAlias
// and not already in use in state. Returns "" when no displayName is
// available or a valid slug cannot be derived.
//
// Collision resolution order:
//  1. slug(displayName)
//  2. slug(displayName) + "-" + slug(orgName)   (if orgName yields a useful suffix)
//  3. slug(displayName) + "-2", "-3", …
func SuggestAlias(state *store.State, displayName, orgName string) string {
	base := slugify(displayName)
	if base == "" {
		return ""
	}
	if store.ValidateAlias(base) != nil {
		return ""
	}

	// Try bare slug first.
	if state.FindByAlias(base) == 0 {
		return base
	}

	// Try base + org suffix when the org name gives a truly distinct token.
	// Skip when the org slug starts with the base (e.g. "Rishabh Anand" for
	// a user named "Rishabh" — the org is just the person's own name and adds
	// no disambiguation value).
	orgSlug := slugify(orgName)
	if orgSlug != "" && orgSlug != base && !strings.HasPrefix(orgSlug, base) {
		candidate := base + "-" + orgSlug
		if len([]rune(candidate)) > 20 {
			candidate = string([]rune(candidate)[:20])
			candidate = strings.TrimRight(candidate, "-")
		}
		if store.ValidateAlias(candidate) == nil && state.FindByAlias(candidate) == 0 {
			return candidate
		}
	}

	// Numeric suffix fallback.
	for i := 2; i <= 99; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if len([]rune(candidate)) > 20 {
			// Trim base to make room for the suffix.
			suffix := fmt.Sprintf("-%d", i)
			trimmed := string([]rune(base)[:20-len([]rune(suffix))])
			candidate = strings.TrimRight(trimmed, "-") + suffix
		}
		if store.ValidateAlias(candidate) == nil && state.FindByAlias(candidate) == 0 {
			return candidate
		}
	}
	return ""
}
