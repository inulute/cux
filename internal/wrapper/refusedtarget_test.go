package wrapper

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// Issue #48: a seat that reads usable from a stale cache but cannot
// actually take the credentials used to end the session outright —
// SwitchTo's error returned straight out of Run(), and fourteen
// conversations died at once because one cancelled account still looked
// healthy in a ten-day-old reading.
//
// Target selection stays fail-open on staleness by design (#46). What
// changes here is that being wrong about it is survivable.

func freshUsage(five, seven float64) usage.AccountUsage {
	u := accountUsage(five, seven)
	u.PolledAt = time.Now().UTC()
	return u
}

// swapFixture stands up a pool with slot 1 live, plus whatever other
// seats the caller names, and stubs out the two things completeSwap
// cannot do for real in a test.
func swapFixture(t *testing.T, accounts map[int]store.Account, cache usage.Cache) (*[]string, *[]time.Duration) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))

	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := accounts[1].Email
	if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"`+live+`","accountUuid":"u-live"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	slots := make([]int, 0, len(accounts))
	for slot := range accounts {
		slots = append(slots, slot)
	}
	state := &store.State{ActiveSlot: 1, Sequence: slots, Accounts: accounts}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(cache); err != nil {
		t.Fatal(err)
	}

	var attempts []string
	var slept []time.Duration
	realSwitch, realSleep := switchTo, sleepFor
	t.Cleanup(func() { switchTo, sleepFor = realSwitch, realSleep })
	sleepFor = func(d time.Duration) { slept = append(slept, d) }
	// Deliberately not recording: every test installs its own switchTo, so
	// attempts has exactly one writer and its count means one thing.
	switchTo = func(string) (store.Account, store.Account, error) {
		return store.Account{}, store.Account{}, errors.New("stub not configured")
	}
	return &attempts, &slept
}

func threeSeats() map[int]store.Account {
	return map[int]store.Account{
		1: {Slot: 1, Email: "live@x.test"},
		2: {Slot: 2, Email: "b@x.test"},
		3: {Slot: 3, Email: "c@x.test"},
	}
}

func rateLimit() *pending {
	return &pending{trigger: history.TriggerRateLimit, reason: "usage limit"}
}

// TestCompleteSwapDropsARefusedTargetAndTakesTheNext is the fix for #48
// on the decision side: the first candidate cannot take the credentials,
// so it is excluded and the choice is made again rather than abandoned.
func TestCompleteSwapDropsARefusedTargetAndTakesTheNext(t *testing.T) {
	accounts := threeSeats()
	attempts, _ := swapFixture(t, accounts, usage.Cache{
		"live@x.test": freshUsage(100, 100),
		"b@x.test":    freshUsage(10, 10),
		"c@x.test":    freshUsage(10, 10),
	})
	// Refuse whichever seat is offered first — the test must not depend on
	// which of the two healthy candidates the strategy prefers.
	switchTo = func(id string) (store.Account, store.Account, error) {
		*attempts = append(*attempts, id)
		if len(*attempts) == 1 {
			return store.Account{}, store.Account{}, errors.New("target credentials missing")
		}
		acct, err := (&store.State{Accounts: accounts}).Resolve(id)
		if err != nil {
			t.Fatalf("stub handed an unresolvable target %q", id)
		}
		return accounts[1], acct, nil
	}

	cfg := config.Defaults()
	retries := 0
	var out bytes.Buffer
	from, to, swapped := completeSwap(rateLimit(), &cfg, true, &retries, &out)

	if !swapped {
		t.Fatalf("a refused target ended the swap; want the next candidate taken (out: %s)", out.String())
	}
	if len(*attempts) != 2 {
		t.Fatalf("SwitchTo attempts = %v, want two (the refused seat, then the next)", *attempts)
	}
	if (*attempts)[0] == (*attempts)[1] {
		t.Fatalf("retried the same refused target %q", (*attempts)[0])
	}
	if from.Email != "live@x.test" || to.Email == "" || to.Email == "live@x.test" {
		t.Fatalf("swapped %s → %s, want a move off the live seat", from.Email, to.Email)
	}
	if !strings.Contains(out.String(), "cannot switch to") {
		t.Errorf("the refusal was not narrated: %s", out.String())
	}
}

// TestCompleteSwapTakesAWorkingTarget is the control. Without it, "the
// session survived" cannot be told apart from "swapping is broken".
func TestCompleteSwapTakesAWorkingTarget(t *testing.T) {
	accounts := threeSeats()
	attempts, slept := swapFixture(t, accounts, usage.Cache{
		"live@x.test": freshUsage(100, 100),
		"b@x.test":    freshUsage(10, 10),
		"c@x.test":    freshUsage(10, 10),
	})
	switchTo = func(id string) (store.Account, store.Account, error) {
		*attempts = append(*attempts, id)
		acct, _ := (&store.State{Accounts: accounts}).Resolve(id)
		return accounts[1], acct, nil
	}

	cfg := config.Defaults()
	retries := 3
	var out bytes.Buffer
	_, to, swapped := completeSwap(rateLimit(), &cfg, true, &retries, &out)

	if !swapped || to.Email == "live@x.test" {
		t.Fatalf("want a completed swap off the live seat, got swapped=%v to=%q", swapped, to.Email)
	}
	if len(*attempts) != 1 {
		t.Fatalf("SwitchTo attempts = %v, want exactly one", *attempts)
	}
	if len(*slept) != 0 {
		t.Errorf("a successful swap slept %v; backoff is for failure only", *slept)
	}
	if retries != 0 {
		t.Errorf("in-place backoff = %d after a successful swap, want it reset", retries)
	}
}

// TestCompleteSwapSurvivesAPoolWithNowhereToGo is #48's headline: the
// only other seat refuses, wait_for_reset is off, and the session must
// still be alive on the other side — held on the current account behind a
// backoff, not returned out of the wrapper.
func TestCompleteSwapSurvivesAPoolWithNowhereToGo(t *testing.T) {
	accounts := map[int]store.Account{
		1: {Slot: 1, Email: "live@x.test"},
		2: {Slot: 2, Email: "cancelled@x.test"},
	}
	attempts, slept := swapFixture(t, accounts, usage.Cache{
		"live@x.test": freshUsage(100, 100),
		// The reading that caused this: months out of date, still plausible.
		"cancelled@x.test": freshUsage(12, 40),
	})
	switchTo = func(id string) (store.Account, store.Account, error) {
		*attempts = append(*attempts, id)
		return store.Account{}, store.Account{}, errors.New("target credentials missing")
	}

	cfg := config.Defaults()
	cfg.WaitForReset = false
	retries := 0
	var out bytes.Buffer
	_, to, swapped := completeSwap(rateLimit(), &cfg, false, &retries, &out)

	if swapped {
		t.Fatal("nothing could take the credentials; want no swap claimed")
	}
	if to.Email != "live@x.test" {
		t.Errorf("held on %q, want the seat the session is already using", to.Email)
	}
	if len(*attempts) != 1 {
		t.Fatalf("SwitchTo attempts = %v, want one — the refused seat must not be retried", *attempts)
	}
	if len(*slept) != 1 || (*slept)[0] <= 0 {
		t.Fatalf("slept %v, want one real backoff before relaunching into the same wall", *slept)
	}
	if retries != 1 {
		t.Errorf("in-place backoff = %d, want it advanced so a repeat waits longer", retries)
	}
}

// TestCompleteSwapBacksOffWhenNoTargetResolves covers the same exit from
// the other direction: nothing is even offered, wait_for_reset is off, and
// the loop must not hand claude straight back to a closed window.
func TestCompleteSwapBacksOffWhenNoTargetResolves(t *testing.T) {
	accounts := map[int]store.Account{
		1: {Slot: 1, Email: "live@x.test"},
		2: {Slot: 2, Email: "alsofull@x.test"},
	}
	attempts, slept := swapFixture(t, accounts, usage.Cache{
		"live@x.test":     freshUsage(100, 100),
		"alsofull@x.test": freshUsage(100, 100),
	})

	cfg := config.Defaults()
	cfg.WaitForReset = false
	retries := 0
	var out bytes.Buffer
	_, _, swapped := completeSwap(rateLimit(), &cfg, false, &retries, &out)

	if swapped {
		t.Fatal("no seat had room; want no swap claimed")
	}
	if len(*attempts) != 0 {
		t.Fatalf("SwitchTo was called %v with nothing to switch to", *attempts)
	}
	if len(*slept) != 1 {
		t.Fatalf("slept %v, want exactly one backoff — the loop must not spin", *slept)
	}
	// Two rounds in a row must wait longer than one, or a persistent
	// outage becomes a fixed-interval relaunch storm across every session.
	*slept = nil
	completeSwap(rateLimit(), &cfg, false, &retries, &out)
	if len(*slept) != 1 || (*slept)[0] < fibonacciDelay(1) {
		t.Errorf("second round slept %v, want one wait of at least %v", *slept, fibonacciDelay(1))
	}
}

// TestExcludeTargetRefusesToRepeat is the loop-breaker. completeSwap only
// retries while a refused target can actually be dropped; a target it
// cannot resolve, or has already dropped, must report false so the caller
// falls through to a backoff instead of asking for it again forever.
func TestExcludeTargetRefusesToRepeat(t *testing.T) {
	swapFixture(t, threeSeats(), usage.Cache{})
	excluded := map[int]bool{}

	if !excludeTarget("2", excluded) {
		t.Fatal("first exclusion of a resolvable seat should take")
	}
	if excludeTarget("2", excluded) {
		t.Error("excluding the same seat twice would loop completeSwap forever")
	}
	if excludeTarget("nobody@x.test", excluded) {
		t.Error("an unresolvable target cannot be excluded, so it must report false")
	}
	if !excluded[2] {
		t.Error("slot 2 should be recorded as refused")
	}
}

// TestCompleteSwapKeepsASwapThatOnlyFailedToRecordItself: SwitchTo names
// both seats when the credentials are already live and only state.json
// failed to save. Treating that error like a refusal would move a session
// that has already moved, and burn a second seat doing it — and before
// #48 it did worse, returning out of the wrapper on a swap that worked.
func TestCompleteSwapKeepsASwapThatOnlyFailedToRecordItself(t *testing.T) {
	accounts := threeSeats()
	attempts, _ := swapFixture(t, accounts, usage.Cache{
		"live@x.test": freshUsage(100, 100),
		"b@x.test":    freshUsage(10, 10),
		"c@x.test":    freshUsage(10, 10),
	})
	switchTo = func(id string) (store.Account, store.Account, error) {
		*attempts = append(*attempts, id)
		acct, _ := (&store.State{Accounts: accounts}).Resolve(id)
		return accounts[1], acct, errors.New("swap complete but state save failed: disk full")
	}

	cfg := config.Defaults()
	retries := 0
	var out bytes.Buffer
	_, to, swapped := completeSwap(rateLimit(), &cfg, true, &retries, &out)

	if !swapped {
		t.Fatalf("the credentials were already live; want the swap kept (out: %s)", out.String())
	}
	if len(*attempts) != 1 {
		t.Fatalf("SwitchTo attempts = %v, want one — a completed swap must not be retried", *attempts)
	}
	if to.Email == "live@x.test" || to.Email == "" {
		t.Errorf("to = %q, want the seat SwitchTo reported", to.Email)
	}
	if !strings.Contains(out.String(), "state did not save") {
		t.Errorf("the bookkeeping failure was not surfaced: %s", out.String())
	}
}
