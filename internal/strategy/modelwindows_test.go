package strategy

import (
	"testing"

	"github.com/inulute/cux/internal/usage"
)

func mw(five, seven float64, opus *float64) usage.AccountUsage {
	u := usage.AccountUsage{
		FiveHour: &usage.Window{Utilization: five},
		SevenDay: &usage.Window{Utilization: seven},
	}
	if opus != nil {
		u.SevenDayOpus = &usage.Window{Utilization: *opus}
	}
	return u
}

func pct(v float64) *float64 { return &v }

func TestDrainPrefersModelClearCandidate(t *testing.T) {
	// b ranks first by 7d-drain order but its Opus window is capped;
	// c is model-clear and must win despite ranking later.
	a := Candidate{Email: "a@x.test", CacheKey: "a"}
	b := Candidate{Email: "b@x.test", CacheKey: "b"}
	c := Candidate{Email: "c@x.test", CacheKey: "c"}
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}
	cache := usage.Cache{
		"a": mw(100, 80, nil),     // current, exhausted
		"b": mw(10, 60, pct(100)), // healthiest overall but Opus-capped
		"c": mw(10, 30, nil),      // model-clear
	}
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b, c}, a, cache, th)
	if !ok || pick.Email != "c@x.test" {
		t.Errorf("got (%q, %v), want the model-clear c@x.test", pick.Email, ok)
	}
}

func TestDrainFallsBackWhenAllModelCapped(t *testing.T) {
	// Every candidate is model-capped → the second sweep must restore
	// today's pick instead of stranding the pool.
	a := Candidate{Email: "a@x.test", CacheKey: "a"}
	b := Candidate{Email: "b@x.test", CacheKey: "b"}
	c := Candidate{Email: "c@x.test", CacheKey: "c"}
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}
	cache := usage.Cache{
		"a": mw(100, 80, pct(100)),
		"b": mw(10, 60, pct(100)),
		"c": mw(20, 30, pct(100)),
	}
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b, c}, a, cache, th)
	if !ok {
		t.Fatal("expected a fallback pick, pool must never be stranded by the preference")
	}
	if pick.Email != "b@x.test" {
		t.Errorf("fallback picked %q, want b@x.test (today's drain order)", pick.Email)
	}
}

func TestDrainUnchangedWhenModelWindowsAbsent(t *testing.T) {
	// Plans that do not report model windows (nil) must behave exactly
	// as before the preference existed.
	a := Candidate{Email: "a@x.test", CacheKey: "a"}
	b := Candidate{Email: "b@x.test", CacheKey: "b"}
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}
	cache := usage.Cache{
		"a": mw(100, 80, nil),
		"b": mw(10, 60, nil),
	}
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b}, a, cache, th)
	if !ok || pick.Email != "b@x.test" {
		t.Errorf("got (%q, %v), want b@x.test unchanged", pick.Email, ok)
	}
}

func TestBalancedSortsModelCappedLastButEligible(t *testing.T) {
	a := Candidate{Email: "a@x.test", CacheKey: "a"}
	b := Candidate{Email: "b@x.test", CacheKey: "b"}
	c := Candidate{Email: "c@x.test", CacheKey: "c"}
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}
	cache := usage.Cache{
		"a": mw(100, 80, nil),    // current
		"b": mw(5, 10, pct(100)), // lowest 7d but Opus-capped
		"c": mw(5, 40, nil),      // model-clear, higher 7d
	}
	pick, ok := PickNext(KindBalanced, nil, []Candidate{a, b, c}, a, cache, th)
	if !ok || pick.Email != "c@x.test" {
		t.Errorf("got (%q, %v), want model-clear c@x.test first", pick.Email, ok)
	}

	// b alone must still be pickable — capped means deprioritised, not
	// ineligible.
	pick, ok = PickNext(KindBalanced, nil, []Candidate{a, b}, a, cache, th)
	if !ok || pick.Email != "b@x.test" {
		t.Errorf("got (%q, %v), want b@x.test as the only candidate", pick.Email, ok)
	}
}

func TestRebalanceRefusesModelCappedPriority(t *testing.T) {
	priority := Candidate{Email: "prio@x.test", CacheKey: "p"}
	temp := Candidate{Email: "temp@x.test", CacheKey: "t"}
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}
	cache := usage.Cache{
		"p": mw(5, 10, pct(100)), // healthy overall, Opus-capped
		"t": mw(20, 30, nil),
	}
	// Rebalance is proactive: hopping onto a model-capped seat trades a
	// working account for an instant rate limit and a bounce back.
	if pick, ok := ShouldRebalance(KindDrain, []string{"prio@x.test"},
		[]Candidate{priority, temp}, temp, cache, th); ok {
		t.Errorf("expected no rebalance onto model-capped priority, got %q", pick.Email)
	}

	// Once the model window resets, the rebalance resumes.
	cache["p"] = mw(5, 10, pct(40))
	if pick, ok := ShouldRebalance(KindDrain, []string{"prio@x.test"},
		[]Candidate{priority, temp}, temp, cache, th); !ok || pick.Email != "prio@x.test" {
		t.Errorf("got (%q, %v), want rebalance to prio@x.test", pick.Email, ok)
	}
}

func TestModelCappedReadsSonnetWindowToo(t *testing.T) {
	u := mw(5, 10, nil)
	u.SevenDaySonnet = &usage.Window{Utilization: 100}
	cache := usage.Cache{"k": u}
	if !modelCapped(cache, "k") {
		t.Error("sonnet window at 100%% must count as model-capped")
	}
	if modelCapped(cache, "missing") {
		t.Error("missing cache entry must not read as capped")
	}
}
