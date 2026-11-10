// Package monitor stitches store, creds, and usage together to keep
// the on-disk usage cache fresh.
//
// Call sites:
//   - The wrapper triggers RefreshActiveCoalesced(email, …) after each Stop
//     signal so the cache mirrors reality without flooding the API — many
//     sessions ending turns at once collapse to one fetch.
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
	"github.com/inulute/cux/internal/profile"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

const lockTimeout = 10 * time.Second

// RefreshAll fetches usage for every managed account and writes the
// merged cache to disk. Returns the resulting cache plus any per-account
// errors so the caller can surface them without aborting the whole
// refresh — one expired token shouldn't poison the others.
func RefreshAll() (usage.Cache, []error) {
	return refreshAll(0)
}

// RefreshAllCoalesced refreshes only the accounts whose cached reading is
// missing or older than maxAge. Because the refresh holds a cross-process
// file lock, N sessions that all poll within the same maxAge window
// collapse to a single API sweep: the first through the lock fetches, the
// rest find a fresh-enough cache and skip. This is the relief for the
// usage endpoint 429ing once many cux terminals poll at once (issue #39,
// related to #21). A maxAge <= 0 is identical to RefreshAll — every
// account is fetched — so freshness-critical callers keep using RefreshAll.
func RefreshAllCoalesced(maxAge time.Duration) (usage.Cache, []error) {
	return refreshAll(maxAge)
}

// freshEnough reports whether an entry polled at polledAt is recent enough
// that a coalesced refresh may skip re-fetching it. A zero polledAt (never
// polled) or a non-positive maxAge (coalescing off) is never fresh enough,
// so those always fetch.
func freshEnough(polledAt time.Time, maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 || polledAt.IsZero() {
		return false
	}
	return now.Sub(polledAt) < maxAge
}

func refreshAll(maxAge time.Duration) (usage.Cache, []error) {
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
	// Repair seat identities first: a backfill changes cache keys, so it has
	// to happen before anything is looked up or written under the old ones.
	// This holds the state lock already, which is what the repair needs.
	repaired := false
	if renames := state.BackfillIdentities(); len(renames) > 0 {
		for old, current := range renames {
			u, had := cache[old]
			delete(cache, old)
			if had && current != "" {
				cache[current] = u
			}
		}
		repaired = true
	}
	// Fetch all accounts in parallel — latency is dominated by the API
	// round-trip, so N sequential calls cost N× more than needed. Under
	// coalescing (maxAge > 0), skip any account another session already
	// refreshed within maxAge — the shared cache holds a current-enough
	// reading, gated per account so a newly-added or last-errored seat is
	// still fetched.
	now := time.Now()
	type result struct {
		cacheKey string
		entry    usage.AccountUsage
		err      error
		email    string
	}
	ch := make(chan result, len(state.Accounts))
	var wg sync.WaitGroup
	fetched := 0
	var cooling []string
	for slot, a := range state.Accounts {
		if u, ok := cache[a.CacheKey()]; ok {
			if freshEnough(u.PolledAt, maxAge, now) {
				continue
			}
			// Held back after a refusal, and deliberately regardless of
			// maxAge: an uncoalesced caller wanting a current reading is
			// exactly who kept re-asking the seat that could not answer.
			if u.InCooldown(now) {
				cooling = append(cooling, fmt.Sprintf("%s: rate limited, next poll %s",
					a.Email, u.RetryAfter.Local().Format("15:04:05")))
				continue
			}
		}
		fetched++
		wg.Add(1)
		others := make([]store.Account, 0, len(state.Accounts))
		for otherSlot, other := range state.Accounts {
			if otherSlot != slot {
				others = append(others, other)
			}
		}
		go func(slot int, a store.Account, others []store.Account) {
			defer wg.Done()
			entry, err := refreshOne(state, a, others)
			ch <- result{cacheKey: a.CacheKey(), entry: entry, err: err, email: a.Email}
		}(slot, a, others)
	}
	wg.Wait()
	close(ch)

	var errs []error
	dirty := repaired
	for r := range ch {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.email, r.err))
			// On token-expired we still cache the marker so `cux list`
			// surfaces the state to the user.
			if r.entry.TokenExpired {
				cache[r.cacheKey] = r.entry
				dirty = true
				continue
			}
			// Every other failure is recorded against the reading we already
			// hold, so the next caller can see that this account was asked
			// and refused. Without it PolledAt stays at the last success, the
			// entry reads stale forever, and staleness is what makes the next
			// caller fetch — the seat that fails is then the one polled
			// hardest (#53).
			kept := cache[r.cacheKey]
			kept.Failures++
			var limited *usage.RateLimitedError
			named := time.Duration(0)
			if errors.As(r.err, &limited) {
				named = limited.RetryAfter
			}
			kept.RetryAfter = kept.NextRetry(time.Now(), named)
			cache[r.cacheKey] = kept
			dirty = true
			continue
		}
		// A success clears the hold-off; the account is answering again.
		r.entry.RetryAfter, r.entry.Failures = time.Time{}, 0
		cache[r.cacheKey] = r.entry
		dirty = true
	}
	for _, c := range cooling {
		errs = append(errs, errors.New(c))
	}
	// When nothing was fetched or repaired the cache is untouched, so skip
	// the write entirely — that no-op is the whole point of coalescing. A
	// recorded refusal counts as a change: it is what stops the next caller
	// asking the same account again.
	if dirty {
		if err := usage.SaveCache(cache); err != nil {
			errs = append(errs, err)
		}
	}
	if repaired {
		if err := state.Save(); err != nil {
			errs = append(errs, fmt.Errorf("monitor: save repaired identities: %w", err))
		}
	}
	return cache, errs
}

// RefreshActive refreshes one account by email, unconditionally.
func RefreshActive(email string) error { return refreshActive(email, 0) }

// RefreshActiveCoalesced refreshes one account unless another session
// already polled it within maxAge.
//
// The wrapper calls this after every Stop signal, so on a busy host the
// request rate is (sessions × turns), all aimed at the one account they
// share — which is why the *active* seat 429s continuously while an idle
// seat in the same pool refreshes normally (#53). The lock alone does not
// help: it serialises those fetches, it does not remove them.
//
// maxAge is a genuine trade, not free deduplication. This reading feeds
// threshold evaluation on the very next line of the wrapper's Stop
// handler, so a wide window hands stale utilisation to the decision that
// is already criticised for arriving late (#49). Keep it near
// refreshCoalesceWindow: long enough that N sessions ending turns together
// collapse to one fetch, short enough that no threshold check ever reasons
// about a materially older number than it does today.
func RefreshActiveCoalesced(email string, maxAge time.Duration) error {
	return refreshActive(email, maxAge)
}

func refreshActive(email string, maxAge time.Duration) error {
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
	cache, cacheErr := usage.LoadCache()
	if cacheErr != nil {
		return cacheErr
	}
	if cache == nil {
		cache = usage.Cache{}
	}
	// Checked under the lock, so of N sessions arriving together exactly one
	// fetches and the rest find its reading already written.
	if u, ok := cache[acct.CacheKey()]; ok && freshEnough(u.PolledAt, maxAge, time.Now()) {
		return nil
	}

	others := make([]store.Account, 0, len(state.Accounts))
	for otherSlot, other := range state.Accounts {
		if otherSlot != slot {
			others = append(others, other)
		}
	}
	entry, err := refreshOne(state, acct, others)
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
//
// refreshOne polls one account. others is the rest of the managed pool, used
// only on the post-401 fallback path to check a live token is not already
// filed under another slot; pass nil to skip that check.
//
// A refresh rotates the seat's refresh token and revokes the old one, so it
// is only safe where cux can hand the new token to every directory holding
// the old one. That is this process's own directory and nothing else: a
// seat whose stored refresh token is live in any other directory cux can
// find — the shared login seen from a terminal-scoped process, or a seat's
// own ~/.claude-<alias> profile seen from the shared terminal — is polled
// with that directory's token and never refreshed from here. Its claude
// refreshes it. A login a profile's claude refreshed on its own is taken
// into the backup first (profile.Reconcile), and an expired backup is not
// refreshed while another directory holds a newer login of the seat.
func refreshOne(state *store.State, acct store.Account, others []store.Account) (usage.AccountUsage, error) {
	slot, email, orgUUID := acct.Slot, acct.Email, acct.OrgUUID
	if _, err := profile.Reconcile(state, acct); err != nil {
		return usage.AccountUsage{}, fmt.Errorf("monitor: take the profile's login into the backup: %w", err)
	}
	syncCurrent := func(b string) error {
		_, err := syncLiveIfActive(email, orgUUID, b)
		return err
	}
	blob, err := creds.ReadBackup(slot, email)
	if err != nil {
		return usage.AccountUsage{}, err
	}
	own := os.Getenv(paths.EnvClaudeConfigDir)
	holder, held, err := profile.HeldElsewhere(own, blob)
	if err != nil {
		return usage.AccountUsage{}, fmt.Errorf("monitor: %w", err)
	}
	if held {
		return pollHeldElsewhere(slot, holder)
	}

	// Proactively refresh before we even try the API if the token is
	// expired or within the 5-minute buffer window.
	if creds.IsTokenExpired(blob) {
		// Another directory's claude refreshed the seat since cux saw it, so
		// the backup's token is most likely revoked, and every poll would
		// try it again until the shared terminal adopts the new one. Poll
		// with that directory's token instead. Not when this process's own
		// login is the backup's: that one is refreshed here and handed over.
		if ownLive, _ := creds.ReadLiveIn(own); !creds.SameLogin(ownLive, blob) {
			if newer, ok, err := profile.NewerElsewhere(state, slot, own, blob); err != nil {
				return usage.AccountUsage{}, fmt.Errorf("monitor: %w", err)
			} else if ok {
				return pollHeldElsewhere(slot, newer)
			}
		}
		if freshBlob, refreshErr := refreshBlob(blob); refreshErr == nil {
			if writeErr := creds.WriteBackup(slot, email, freshBlob); writeErr != nil {
				// If we can't persist the new token, the refresh token may
				// be single-use (OAuth 2.1) and we'd permanently lose access.
				// Treat this as fatal rather than silently proceeding with a
				// token we cannot save.
				return usage.AccountUsage{}, fmt.Errorf("monitor: save refreshed token: %w", writeErr)
			}
			// Recorded before the token is handed on, so a directory the
			// handover misses keeps a login a later switch can recognise as
			// revoked and repair (profile.NeedsCopy). The record is
			// best-effort: the refreshed token is already saved, and a
			// failure to record costs only that later repair, not this
			// poll's reading.
			_ = creds.RecordRevoked(slot, email, blob, freshBlob)
			if writeErr := syncCurrent(freshBlob); writeErr != nil {
				return usage.AccountUsage{}, fmt.Errorf("monitor: save refreshed live token: %w", writeErr)
			}
			if writeErr := profile.SyncRefreshed(state, acct, blob, freshBlob); writeErr != nil {
				return usage.AccountUsage{}, fmt.Errorf("monitor: save refreshed token into the profile: %w", writeErr)
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
		if writeErr := syncCurrent(blob); writeErr != nil {
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
	// Live token worked, so adopt it into the slot — that is what makes a
	// `claude login` without a `cux add` self-heal on the next refresh.
	//
	// But the check that got us here compared the *identity file* against
	// this slot's email, and that file is exactly what can disagree with the
	// credential store (issue #46). Passing it does not establish that the
	// live token belongs to this account, so adopting on that basis alone is
	// how one account's token ends up filed under another's name — the same
	// corruption `cux add` and `cux switch` now refuse, arriving through a
	// background refresh instead of a command.
	//
	// So prove it negatively before writing: if this token is already stored
	// under a different slot, it is not this account's. The scan reads the
	// other slots' backups, which is why it lives here on the post-401 path
	// rather than in the refresh proper — a dead stored token is rare, while
	// refreshAll runs at every session start, Stop signal and idle check.
	//
	// On a collision, keep the reading and skip the write. The figure is
	// still worth having, and leaving the stale backup in place keeps the
	// slot repairable by `cux add`, which can attribute the token properly.
	// `cux status` reports pools already in this state.
	//
	// Under a terminal scope this process's directory is a profile, which
	// may hold a login of this seat made there on its own while the backup
	// follows another directory. Adopting it then would let that other
	// directory's next sync copy it, giving one token two holders; adopt
	// only when the profile is the seat's sole holder.
	if profile.TerminalScoped() {
		if _, elsewhere, lerr := profile.LiveElsewhere(state, slot, os.Getenv(paths.EnvClaudeConfigDir)); lerr != nil || elsewhere {
			return u2, nil
		}
	}
	refs := make([]creds.SlotRef, 0, len(others))
	for _, o := range others {
		refs = append(refs, creds.SlotRef{Slot: o.Slot, Email: o.Email})
	}
	if _, shared := creds.SlotHoldingToken(refs, slot, liveBlob); !shared {
		// Best-effort: if this fails we still return the valid usage data.
		_ = creds.WriteBackup(slot, email, liveBlob)
	}
	return u2, nil
}

// refreshBlob is creds.RefreshBlob; tests replace it to count refreshes.
var refreshBlob = creds.RefreshBlob

// pollHeldElsewhere reads a seat's usage with the token of the directory
// that runs on it. It never refreshes: that would revoke the token the
// directory's claude holds. When the token has expired the last reading is
// kept until that claude refreshes it.
func pollHeldElsewhere(slot int, h profile.Holder) (usage.AccountUsage, error) {
	if creds.IsTokenExpired(h.Blob) {
		return usage.AccountUsage{}, fmt.Errorf("monitor: slot %d is logged in at %s, whose claude refreshes it; "+
			"its access token has expired, so the last reading stays until that claude runs", slot, profile.Label(h.Dir))
	}
	token, err := creds.ExtractAccessToken(h.Blob)
	if err != nil {
		return usage.AccountUsage{}, err
	}
	return usage.Fetch(token)
}

// syncLiveIfActive writes blob into this process's live login when that
// login is the same account. Under a terminal scope this process's
// directory is a profile, and profile.SyncRefreshed — which also checks the
// profile holds the token being replaced — is what writes there.
func syncLiveIfActive(email, orgUUID, blob string) (bool, error) {
	if email == "" || blob == "" || profile.TerminalScoped() {
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
