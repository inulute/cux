package wrapper

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// staleAge is comfortably past usage.StaleAfter — the 12.8 days reported in
// issue #46.
const staleAge = 307 * time.Hour

func agedUsage(five, seven float64, age time.Duration) usage.AccountUsage {
	u := accountUsage(five, seven)
	u.PolledAt = time.Now().UTC().Add(-age)
	return u
}

// stalenessFixture stands up a two-account pool with a@x.test live, and
// writes cache readings of the given age.
func stalenessFixture(t *testing.T, age time.Duration, activeFive, activeSeven float64) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claudeDir, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"a@x.test","accountUuid":"u1"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	state := &store.State{
		ActiveSlot: 1,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": agedUsage(activeFive, activeSeven, age),
		"b@x.test": agedUsage(0, 20, age),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEvaluateThresholdSwapIgnoresAStaleReading is the core of issue #46 on
// the decision side: the pool looks exhausted, but the reading predates any
// evidence for that, and a threshold swap costs a process restart (#39).
func TestEvaluateThresholdSwapIgnoresAStaleReading(t *testing.T) {
	stalenessFixture(t, staleAge, 100, 67)
	cfg := config.Defaults()
	cfg.Thresholds = usage.Thresholds{FiveHour: 98, SevenDay: 98}
	if p := evaluateThresholdSwap(&cfg, ""); p != nil {
		t.Fatalf("swapped on a %s-old reading: %+v", staleAge, p)
	}
}

// TestEvaluateThresholdSwapFiresOnAFreshReading is the control. Without it,
// "no swap happened" cannot be told apart from "swapping is broken".
func TestEvaluateThresholdSwapFiresOnAFreshReading(t *testing.T) {
	stalenessFixture(t, time.Minute, 100, 67)
	cfg := config.Defaults()
	cfg.Thresholds = usage.Thresholds{FiveHour: 98, SevenDay: 98}
	p := evaluateThresholdSwap(&cfg, "")
	if p == nil {
		t.Fatal("a fresh hard-limit reading should still swap")
	}
	if p.explicitTarget != "2" {
		t.Fatalf("target = %q, want slot \"2\"", p.explicitTarget)
	}
}

// TestPrelaunchOnAStaleCacheNeitherSwapsNorBlocks is the whole-system
// assertion: with every reading long past its shelf life, cux must not move
// the session and must not refuse to launch it either. Uncertainty is not
// exhaustion (issues #37, #39).
func TestPrelaunchOnAStaleCacheNeitherSwapsNorBlocks(t *testing.T) {
	stalenessFixture(t, staleAge, 100, 100)
	// Both accounts exhausted-looking, all of it unverifiable.
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": agedUsage(100, 100, staleAge),
		"b@x.test": agedUsage(100, 100, staleAge),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	p, blocked := evaluatePrelaunchHardLimitSwap(&cfg)
	if p != nil {
		t.Fatalf("preflight swapped on stale data: %+v", p)
	}
	if blocked != "" {
		t.Fatalf("preflight blocked the launch on stale data: %s", blocked)
	}
}

// TestPrelaunchStillBlocksOnAFreshExhaustedPool is that test's control.
func TestPrelaunchStillBlocksOnAFreshExhaustedPool(t *testing.T) {
	stalenessFixture(t, time.Minute, 100, 100)
	if err := usage.SaveCache(usage.Cache{
		"a@x.test": agedUsage(100, 100, time.Minute),
		"b@x.test": agedUsage(100, 100, time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	p, blocked := evaluatePrelaunchHardLimitSwap(&cfg)
	if p != nil {
		t.Fatalf("nowhere to go, yet a swap was proposed: %+v", p)
	}
	if blocked == "" {
		t.Fatal("a genuinely exhausted pool should still block the launch")
	}
}

// TestAllTokensExpiredKeepsWaitingOnAStaleReading guards the one verdict in
// the wrapper that ends a session outright. A TokenExpired flag from days ago
// proves nothing about now — the user may have logged back in, and the poll
// that would show it is the thing failing.
func TestAllTokensExpiredKeepsWaitingOnAStaleReading(t *testing.T) {
	accounts := map[int]store.Account{
		1: {Slot: 1, Email: "a@x.test"},
		2: {Slot: 2, Email: "b@x.test"},
	}
	stale := usage.Cache{
		"a@x.test": {TokenExpired: true, PolledAt: time.Now().UTC().Add(-staleAge)},
		"b@x.test": {TokenExpired: true, PolledAt: time.Now().UTC().Add(-staleAge)},
	}
	if allTokensExpired(accounts, stale) {
		t.Fatal("a stale expiry flag was treated as proof that waiting cannot help")
	}

	fresh := usage.Cache{
		"a@x.test": {TokenExpired: true, PolledAt: time.Now().UTC().Add(-time.Minute)},
		"b@x.test": {TokenExpired: true, PolledAt: time.Now().UTC().Add(-time.Minute)},
	}
	if !allTokensExpired(accounts, fresh) {
		t.Fatal("a freshly confirmed pool-wide expiry should still end the wait")
	}
}
