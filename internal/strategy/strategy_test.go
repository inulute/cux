package strategy

import (
	"testing"

	"github.com/inulute/cux/internal/usage"
	"time"
)

func u(seven float64) usage.AccountUsage {
	w7 := usage.Window{Utilization: seven}
	return usage.AccountUsage{SevenDay: &w7}
}

func expired() usage.AccountUsage {
	uu := u(0)
	uu.TokenExpired = true
	return uu
}

func threeAccounts() []Candidate {
	return []Candidate{{Email: "a@x"}, {Email: "b@x"}, {Email: "c@x"}}
}

func defaultThresholds() usage.Thresholds {
	return usage.DefaultThresholds()
}

// --- ParseKind -----------------------------------------------------------

func TestParseKind(t *testing.T) {
	t.Parallel()
	cases := map[string]Kind{
		"":         KindDrain,
		"drain":    KindDrain,
		"DRAIN":    KindDrain,
		"balanced": KindBalanced,
		"manual":   KindManual,
		"garbage":  KindDrain, // safe default
	}
	for in, want := range cases {
		if got := ParseKind(in); got != want {
			t.Errorf("ParseKind(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- Manual --------------------------------------------------------------

func TestPickNextManual_NeverPicks(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{}
	if _, ok := PickNext(KindManual, nil, accts, accts[0], cache, defaultThresholds(), now()); ok {
		t.Fatal("manual mode must never auto-pick")
	}
}

// --- Balanced ------------------------------------------------------------

func TestPickNextBalanced_PicksLowest7d(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(80), // current
		"b@x": u(30), // lowest 7d
		"c@x": u(60),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("balanced should pick b@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_SkipsExpired(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(80),
		"b@x": expired(),
		"c@x": u(60),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should skip expired b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_NoCacheTreatsAsAvailable(t *testing.T) {
	t.Parallel()
	// Fresh install: cache is empty. Balanced should still pick *some*
	// non-current account so a manual /switch from a fresh user works.
	accts := threeAccounts()
	cache := usage.Cache{}
	if _, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds(), now()); !ok {
		t.Fatal("balanced should still pick when cache is empty")
	}
}

func TestPickNextBalanced_OnlyOneAccount(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}}
	cache := usage.Cache{"a@x": u(80)}
	if _, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds(), now()); ok {
		t.Fatal("balanced should not pick when there are no other accounts")
	}
}

// --- Drain ---------------------------------------------------------------

func TestPickNextDrain_AutoOrder_PicksHighest7dUnderCap(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(40), // current
		"b@x": u(90), // highest 7d that's under 95
		"c@x": u(20),
	}
	pick, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("drain auto should pick highest-7d under cap (b@x); got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_PriorityOrder(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(40), // current
		"b@x": u(30),
		"c@x": u(20),
	}
	order := []string{"c@x", "b@x", "a@x"}
	pick, ok := PickNext(KindDrain, order, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("drain should follow priority order, picking c@x first; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_SkipsAccountOver7dCap(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(40), // current
		"b@x": u(97), // over 7d cap
		"c@x": u(20),
	}
	order := []string{"b@x", "c@x"}
	pick, ok := PickNext(KindDrain, order, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("drain should skip 7d-maxed b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_FallsBackBelowHardLimit(t *testing.T) {
	t.Parallel()
	// Every account is over the 7d soft cap; b@x is still below 100.
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(96),  // current
		"b@x": u(96),  // over soft cap, below hard limit
		"c@x": u(100), // hard-full
	}
	pick, ok := PickNext(KindDrain, []string{"c@x", "b@x"}, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("drain second pass should pick b@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_NeverPicks7DayHardFull(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(96),  // current
		"b@x": u(100), // 7d hard-full
		"c@x": u(100), // 7d hard-full
	}
	if _, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds(), now()); ok {
		t.Fatal("drain should not pick a 7d-hard-full account")
	}
}

func TestPickNextBalanced_NeverPicks7DayHardFull(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}, {Email: "c@x"}}
	cache := usage.Cache{
		"a@x": u(40),  // current
		"b@x": u(100), // 7d hard-full
		"c@x": u(20),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds(), now())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should skip 7d-hard-full b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_NoCandidates(t *testing.T) {
	t.Parallel()
	// Everything is fully maxed.
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(100),
		"b@x": u(100),
		"c@x": u(100),
	}
	if _, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds(), now()); ok {
		t.Fatal("drain should report ok=false when nothing has capacity")
	}
}

// --- Rebalance -----------------------------------------------------------

func TestShouldRebalance_PriorityOrder_ReturnsHealthyPriority(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(40),
		"b@x": u(10),
	}
	pick, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], cache, defaultThresholds(), now())
	if !ok || pick.Email != "a@x" {
		t.Fatalf("on b@x, priority a@x is healthy → rebalance to a@x; got %+v ok=%v", pick, ok)
	}
}

func TestShouldRebalance_OnPriorityAlready(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(40),
		"b@x": u(10),
	}
	if _, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[0], cache, defaultThresholds(), now()); ok {
		t.Fatal("already on priority a@x → no rebalance")
	}
}

func TestShouldRebalance_PriorityStillOverThreshold(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(96), // 7d still over 95 cap
		"b@x": u(10),
	}
	thresholds := usage.Thresholds{SevenDay: 95}
	if _, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], cache, thresholds, now()); ok {
		t.Fatal("priority a@x still over threshold → don't rebalance")
	}
}

func TestShouldRebalance_NotAvailableInDrainMode(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(40),
		"b@x": u(10),
	}
	if _, ok := ShouldRebalance(KindBalanced, nil, accts, accts[1], cache, defaultThresholds(), now()); ok {
		t.Fatal("balanced mode should never rebalance")
	}
	if _, ok := ShouldRebalance(KindManual, nil, accts, accts[1], cache, defaultThresholds(), now()); ok {
		t.Fatal("manual mode should never rebalance")
	}
}

// now is the decision instant the tests evaluate at. Fixtures below carry no
// poll time, so they read as unknown-age rather than stale — the staleness
// gate is exercised separately.
func now() time.Time { return time.Now() }

// TestShouldRebalanceRefusesAStaleCandidate — rebalancing is cux's own
// initiative, and the entire claim is that some other account has recovered.
// A reading too old to support that claim must not buy a process restart.
func TestShouldRebalanceRefusesAStaleCandidate(t *testing.T) {
	accts := []Candidate{{Email: "a@x", Slot: 1}, {Email: "b@x", Slot: 2}}
	stale := usage.Cache{
		"a@x": {SevenDay: &usage.Window{Utilization: 10},
			PolledAt: time.Now().Add(-307 * time.Hour)},
		"b@x": {SevenDay: &usage.Window{Utilization: 30},
			PolledAt: time.Now().Add(-307 * time.Hour)},
	}
	if pick, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], stale, defaultThresholds(), now()); ok {
		t.Fatalf("rebalanced onto a stale candidate: %+v", pick)
	}

	fresh := usage.Cache{
		"a@x": {SevenDay: &usage.Window{Utilization: 10},
			PolledAt: time.Now().Add(-time.Minute)},
		"b@x": {SevenDay: &usage.Window{Utilization: 30},
			PolledAt: time.Now().Add(-time.Minute)},
	}
	if _, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], fresh, defaultThresholds(), now()); !ok {
		t.Fatal("a fresh recovered priority account should still be rebalanced onto")
	}
}

// TestPickNextStillPicksFromAStaleCache is the deliberate asymmetry: PickNext
// answers "where do we go" for a move already decided, most sharply after the
// API has refused the current seat. Refusing to name a target there would
// strand a live session (issues #37, #39).
func TestPickNextStillPicksFromAStaleCache(t *testing.T) {
	accts := []Candidate{{Email: "a@x", Slot: 1}, {Email: "b@x", Slot: 2}}
	stale := usage.Cache{
		"a@x": {SevenDay: &usage.Window{Utilization: 60},
			PolledAt: time.Now().Add(-307 * time.Hour)},
		"b@x": {SevenDay: &usage.Window{Utilization: 20},
			PolledAt: time.Now().Add(-307 * time.Hour)},
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], stale, defaultThresholds(), now())
	if !ok {
		t.Fatal("PickNext must still name a target when every reading is stale")
	}
	if pick.Email != "b@x" {
		t.Fatalf("picked %q, want b@x", pick.Email)
	}
}
