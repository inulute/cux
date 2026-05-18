// Package monitor stitches store, creds, and usage together to keep
// the on-disk usage cache fresh.
//
// Call sites:
//   - The wrapper triggers RefreshActive(email) after each Stop signal
//     so the cache mirrors reality without flooding the API.
//   - The wrapper triggers RefreshAll() once at startup, in a
//     background goroutine, so threshold checks have something to
//     work with on the first turn.
//   - `cux usage refresh` and `cux list` call RefreshAll() on demand.
//
// In v0.2 there is no background polling daemon (deferred to v0.3
// alongside the systemd/launchd integration). All refreshes here are
// triggered, not periodic.
package monitor

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/inulute/cux/internal/claudecfg"
	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

const lockTimeout = 10 * time.Second

// RefreshAll fetches usage for every managed account and writes the
// merged cache to disk. Returns the resulting cache plus any per-account
// errors so the caller can surface them without aborting the whole
// refresh — one expired token shouldn't poison the others.
func RefreshAll() (usage.Cache, []error) {
	if err := os.MkdirAll(paths.BackupRoot(), 0o700); err != nil {
		return nil, []error{fmt.Errorf("monitor: mkdir data dir: %w", err)}
	}
	lk, err := lockfile.Acquire(paths.LockFile(), lockTimeout)
	if err != nil {
		return nil, []error{fmt.Errorf("monitor: acquire lock: %w", err)}
	}
	defer lk.Unlock() //nolint:errcheck

	state, err := store.Load()
	if err != nil {
		return nil, []error{err}
	}
	cache, cacheErr := usage.LoadCache()
	if cacheErr != nil {
		return nil, []error{cacheErr}
	}
	if cache == nil {
		cache = usage.Cache{}
	}
	var errs []error
	for slot, a := range state.Accounts {
		entry, err := refreshOne(slot, a.Email, a.OrgUUID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Email, err))
			// On token-expired we still cache the marker so `cux list`
			// surfaces the state to the user.
			if entry.TokenExpired {
				cache[a.CacheKey()] = entry
			}
			continue
		}
		cache[a.CacheKey()] = entry
	}
	if err := usage.SaveCache(cache); err != nil {
		errs = append(errs, err)
	}
	return cache, errs
}

// RefreshActive refreshes one account by email. Used by the wrapper
// after each Stop signal to keep the active account's cache entry
// current before threshold evaluation.
func RefreshActive(email string) error {
	if email == "" {
		return errors.New("monitor: empty email")
	}
	if err := os.MkdirAll(paths.BackupRoot(), 0o700); err != nil {
		return fmt.Errorf("monitor: mkdir data dir: %w", err)
	}
	lk, err := lockfile.Acquire(paths.LockFile(), lockTimeout)
	if err != nil {
		return fmt.Errorf("monitor: acquire lock: %w", err)
	}
	defer lk.Unlock() //nolint:errcheck

	state, err := store.Load()
	if err != nil {
		return err
	}
	slot := state.FindByEmail(email)
	if slot == 0 {
		return fmt.Errorf("monitor: %s not managed by cux", email)
	}
	acct := state.Accounts[slot]
	entry, err := refreshOne(slot, acct.Email, acct.OrgUUID)
	cache, cacheErr := usage.LoadCache()
	if cacheErr != nil {
		return cacheErr
	}
	if cache == nil {
		cache = usage.Cache{}
	}
	if err != nil && !entry.TokenExpired {
		// Network blips shouldn't blow away the prior entry.
		return err
	}
	cache[acct.CacheKey()] = entry
	return usage.SaveCache(cache)
}

// refreshOne reads the account's stored credentials, refreshes the
// access token if it is expired or near expiry, and queries the usage API.
//
// Token source priority:
//  0. If this account is the one Claude Code is currently running, use
//     the live credentials file instead of the per-account backup. The
//     backup is only rewritten on switch, so for the active account it
//     can be hours stale; Claude Code keeps the live file continuously
//     refreshed. A stale backup access token is routinely rejected by
//     the usage API once Claude Code has rotated the refresh token,
//     which previously left the active account's cache entry frozen.
//  1. If IsTokenExpired: call RefreshBlob (standard OAuth refresh_token flow)
//     and update the backup so the next call is already fresh.
//  2. If the API still returns 401 (e.g. the refresh token itself expired):
//     fall back to the live credentials file, but only when the live account
//     email and orgUUID match, to avoid using a different account's token.
func refreshOne(slot int, email, orgUUID string) (usage.AccountUsage, error) {
	blob, err := creds.ReadBackup(slot, email)
	if err != nil {
		return usage.AccountUsage{}, err
	}

	// Prefer the live credentials for whichever account is currently
	// active — see step 0 above. The backup still serves as the source
	// for every inactive account, and as the fallback below if the live
	// token is itself rejected.
	usingLive := false
	if liveBlob, ok := liveBlobFor(email, orgUUID); ok {
		blob = liveBlob
		usingLive = true
	}

	// Proactively refresh before we even try the API if the token is
	// expired or within the 5-minute buffer window.
	if creds.IsTokenExpired(blob) {
		if freshBlob, refreshErr := creds.RefreshBlob(blob); refreshErr == nil {
			if writeErr := creds.WriteBackup(slot, email, freshBlob); writeErr != nil {
				// If we can't persist the new token, the refresh token may
				// be single-use (OAuth 2.1) and we'd permanently lose access.
				// Treat this as fatal rather than silently proceeding with a
				// token we cannot save.
				return usage.AccountUsage{}, fmt.Errorf("monitor: save refreshed token: %w", writeErr)
			}
			if _, writeErr := syncLiveIfActive(email, orgUUID, freshBlob); writeErr != nil {
				return usage.AccountUsage{}, fmt.Errorf("monitor: save refreshed live token: %w", writeErr)
			}
			blob = freshBlob
		}
		// If RefreshBlob failed, continue with the existing blob — the API
		// call may still work, and if not we fall back to live below.
	}

	token, err := creds.ExtractAccessToken(blob)
	if err != nil {
		return usage.AccountUsage{}, err
	}
	u, err := usage.Fetch(token)
	if err == nil {
		// When the token came from the live file, write it back to the
		// per-account backup so the backup stops rotting and the next
		// refresh has a fresh starting point even if Claude Code is not
		// running.
		if usingLive {
			_ = creds.WriteBackup(slot, email, blob)
		}
		if _, writeErr := syncLiveIfActive(email, orgUUID, blob); writeErr != nil {
			return usage.AccountUsage{}, fmt.Errorf("monitor: save active live token: %w", writeErr)
		}
		return u, nil
	}
	if !u.TokenExpired {
		return u, err
	}

	// Still getting 401. The refresh token itself may be expired, or the
	// account was re-logged-in via `claude login` without a `cux add`.
	// Try the live credentials as a last resort, but only for the account
	// that is currently active (to avoid cross-account token use).
	liveBlob, liveErr := creds.ReadLive()
	if liveErr != nil {
		return u, err
	}
	_, parsed, cfgErr := claudecfg.ReadOAuthBlock()
	if cfgErr != nil || parsed.EmailAddress != email {
		return u, err
	}
	// When orgUUID is set, also verify the live account belongs to the same org.
	if orgUUID != "" && parsed.OrganizationUUID != orgUUID {
		return u, err
	}
	liveToken, tokErr := creds.ExtractAccessToken(liveBlob)
	if tokErr != nil {
		return u, err
	}
	u2, err2 := usage.Fetch(liveToken)
	if err2 != nil {
		return u, err
	}
	// Live token worked — update the backup so the next refresh uses it.
	// Best-effort: if this fails we still return the valid usage data.
	_ = creds.WriteBackup(slot, email, liveBlob)
	return u2, nil
}

// liveBlobFor returns the live credential blob when it belongs to the
// given account — that is, when Claude Code is currently running this
// account. Identity is confirmed against the oauthAccount block in the
// Claude Code config (email, and orgUUID when known) so a different
// account's token can never be returned. The boolean is false, with no
// error, whenever the account is not the active one or the live file
// cannot be read; callers fall back to the per-account backup.
func liveBlobFor(email, orgUUID string) (string, bool) {
	if email == "" {
		return "", false
	}
	_, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil || parsed.EmailAddress != email {
		return "", false
	}
	// When orgUUID is known, also require the org to match so two
	// accounts sharing an email address are never conflated.
	if orgUUID != "" && parsed.OrganizationUUID != orgUUID {
		return "", false
	}
	liveBlob, err := creds.ReadLive()
	if err != nil || liveBlob == "" {
		return "", false
	}
	return liveBlob, true
}

func syncLiveIfActive(email, orgUUID, blob string) (bool, error) {
	if email == "" || blob == "" {
		return false, nil
	}
	_, parsed, err := claudecfg.ReadOAuthBlock()
	if err != nil {
		return false, nil
	}
	if parsed.EmailAddress != email {
		return false, nil
	}
	// When orgUUID is set, also verify the live account is the same org to
	// avoid syncing a refreshed token onto a different account sharing the email.
	if orgUUID != "" && parsed.OrganizationUUID != orgUUID {
		return false, nil
	}
	if err := creds.WriteLive(blob); err != nil {
		return false, err
	}
	return true, nil
}
