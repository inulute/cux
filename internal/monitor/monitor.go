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
	"sync"
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
	// Fetch all accounts in parallel — latency is dominated by the API
	// round-trip, so N sequential calls cost N× more than needed.
	type result struct {
		cacheKey string
		entry    usage.AccountUsage
		err      error
		email    string
	}
	ch := make(chan result, len(state.Accounts))
	var wg sync.WaitGroup
	for slot, a := range state.Accounts {
		wg.Add(1)
		go func(slot int, a store.Account) {
			defer wg.Done()
			entry, err := refreshOne(slot, a.Email, a.OrgUUID)
			ch <- result{cacheKey: a.CacheKey(), entry: entry, err: err, email: a.Email}
		}(slot, a)
	}
	wg.Wait()
	close(ch)

	var errs []error
	for r := range ch {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.email, r.err))
			// On token-expired we still cache the marker so `cux list`
			// surfaces the state to the user.
			if r.entry.TokenExpired {
				cache[r.cacheKey] = r.entry
			}
			continue
		}
		cache[r.cacheKey] = r.entry
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
// Token refresh priority:
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
