package strategy

import (
	"testing"

	"github.com/inulute/cux/internal/usage"
)

func u(five, seven float64) usage.AccountUsage {
	w5 := usage.Window{Utilization: five}
	w7 := usage.Window{Utilization: seven}
	return usage.AccountUsage{FiveHour: &w5, SevenDay: &w7}
}

func expired() usage.AccountUsage {
	uu := u(0, 0)
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
	if _, ok := PickNext(KindManual, nil, accts, accts[0], cache, defaultThresholds()); ok {
		t.Fatal("manual mode must never auto-pick")
	}
}

// --- Balanced ------------------------------------------------------------

func TestPickNextBalanced_PicksLowest7d(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 80), // current
		"b@x": u(50, 30), // lowest 7d
		"c@x": u(10, 60),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("balanced should pick b@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_TiebreaksByLower5h(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 80),
		"b@x": u(50, 30), // 7d 30, 5h 50
		"c@x": u(15, 30), // 7d 30, 5h 15 — wins
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should tiebreak to c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_SkipsExpired(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 80),
		"b@x": expired(),
		"c@x": u(10, 60),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should skip expired b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_SkipsFull5h(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 80),
		"b@x": u(100, 10), // lowest 7d, but unusable now
		"c@x": u(10, 60),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should skip 5h-full b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextBalanced_NoCacheTreatsAsAvailable(t *testing.T) {
	t.Parallel()
	// Fresh install: cache is empty. Balanced should still pick *some*
	// non-current account so a manual /switch from a fresh user works.
	accts := threeAccounts()
	cache := usage.Cache{}
	if _, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds()); !ok {
		t.Fatal("balanced should still pick when cache is empty")
	}
}

func TestPickNextBalanced_OnlyOneAccount(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}}
	cache := usage.Cache{"a@x": u(20, 80)}
	if _, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds()); ok {
		t.Fatal("balanced should not pick when there are no other accounts")
	}
}

// --- Drain ---------------------------------------------------------------

func TestPickNextDrain_AutoOrder_PicksHighest7dUnderCap(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 40), // current
		"b@x": u(10, 90), // highest 7d that's under 95
		"c@x": u(5, 20),
	}
	pick, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("drain auto should pick highest-7d under cap (b@x); got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_PriorityOrder(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 40), // current
		"b@x": u(10, 30),
		"c@x": u(5, 20),
	}
	order := []string{"c@x", "b@x", "a@x"}
	pick, ok := PickNext(KindDrain, order, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("drain should follow priority order, picking c@x first; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_SkipsAccountOver7dCap(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 40), // current
		"b@x": u(10, 97), // over 7d cap
		"c@x": u(5, 20),
	}
	order := []string{"b@x", "c@x"}
	pick, ok := PickNext(KindDrain, order, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("drain should skip 7d-maxed b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_SkipsAccountFull5hEvenWhenUnder7dCap(t *testing.T) {
	t.Parallel()
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 40),  // current
		"b@x": u(100, 10), // under 7d cap but cannot take work
		"c@x": u(10, 20),
	}
	order := []string{"b@x", "c@x"}
	pick, ok := PickNext(KindDrain, order, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("drain should skip 5h-full b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_FallsBackTo5hCapacity(t *testing.T) {
	t.Parallel()
	// Every account is over 7d cap, but b@x has 5h capacity.
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 96), // current
		"b@x": u(50, 96), // 7d maxed but 5h has room
		"c@x": u(95, 96), // 7d and 5h both maxed
	}
	pick, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "b@x" {
		t.Fatalf("drain second pass should pick 5h-availble b@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_NeverPicks7DayHardFull(t *testing.T) {
	t.Parallel()
	// b@x has 5h capacity but 7d is at the hard 100% ceiling.
	// Pass 2 must not select it even though 5h has room.
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(20, 96),  // current
		"b@x": u(50, 100), // 5h has room but 7d hard-full
		"c@x": u(95, 96),  // 5h maxed
	}
	if _, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds()); ok {
		t.Fatal("drain should not pick 7d-hard-full b@x even with 5h room")
	}
}

func TestPickNextBalanced_NeverPicks7DayHardFull(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}, {Email: "c@x"}}
	cache := usage.Cache{
		"a@x": u(20, 40),  // current
		"b@x": u(10, 100), // 7d hard-full
		"c@x": u(30, 20),
	}
	pick, ok := PickNext(KindBalanced, nil, accts, accts[0], cache, defaultThresholds())
	if !ok || pick.Email != "c@x" {
		t.Fatalf("balanced should skip 7d-hard-full b@x and pick c@x; got %+v ok=%v", pick, ok)
	}
}

func TestPickNextDrain_NoCandidates(t *testing.T) {
	t.Parallel()
	// Everything is fully maxed.
	accts := threeAccounts()
	cache := usage.Cache{
		"a@x": u(95, 96),
		"b@x": u(95, 96),
		"c@x": u(95, 96),
	}
	if _, ok := PickNext(KindDrain, nil, accts, accts[0], cache, defaultThresholds()); ok {
		t.Fatal("drain should report ok=false when nothing has capacity")
	}
}

// --- Rebalance -----------------------------------------------------------

func TestShouldRebalance_PriorityOrder_ReturnsHealthyPriority(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(10, 40),
		"b@x": u(30, 10),
	}
	pick, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], cache, defaultThresholds())
	if !ok || pick.Email != "a@x" {
		t.Fatalf("on b@x, priority a@x is healthy → rebalance to a@x; got %+v ok=%v", pick, ok)
	}
}

func TestShouldRebalance_OnPriorityAlready(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(10, 40),
		"b@x": u(30, 10),
	}
	if _, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[0], cache, defaultThresholds()); ok {
		t.Fatal("already on priority a@x → no rebalance")
	}
}

func TestShouldRebalance_PriorityStillOverThreshold(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(92, 40), // 5h still over 90 cap
		"b@x": u(30, 10),
	}
	// Use explicit 90% FiveHour threshold so IsOverThreshold fires for a@x.
	thresholds := usage.Thresholds{FiveHour: 90, SevenDay: 95}
	if _, ok := ShouldRebalance(KindDrain, []string{"a@x", "b@x"}, accts, accts[1], cache, thresholds); ok {
		t.Fatal("priority a@x still over threshold → don't rebalance")
	}
}

func TestShouldRebalance_NotAvailableInDrainMode(t *testing.T) {
	t.Parallel()
	accts := []Candidate{{Email: "a@x"}, {Email: "b@x"}}
	cache := usage.Cache{
		"a@x": u(10, 40),
		"b@x": u(30, 10),
	}
	if _, ok := ShouldRebalance(KindBalanced, nil, accts, accts[1], cache, defaultThresholds()); ok {
		t.Fatal("balanced mode should never rebalance")
	}
	if _, ok := ShouldRebalance(KindManual, nil, accts, accts[1], cache, defaultThresholds()); ok {
		t.Fatal("manual mode should never rebalance")
	}
}
