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
		u.Models = map[string]*usage.Window{"seven_day_opus": {Utilization: *opus}}
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
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b, c}, a, cache, th, now())
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
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b, c}, a, cache, th, now())
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
	pick, ok := PickNext(KindDrain, nil, []Candidate{a, b}, a, cache, th, now())
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
	pick, ok := PickNext(KindBalanced, nil, []Candidate{a, b, c}, a, cache, th, now())
	if !ok || pick.Email != "c@x.test" {
		t.Errorf("got (%q, %v), want model-clear c@x.test first", pick.Email, ok)
	}

	// b alone must still be pickable — capped means deprioritised, not
	// ineligible.
	pick, ok = PickNext(KindBalanced, nil, []Candidate{a, b}, a, cache, th, now())
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
		[]Candidate{priority, temp}, temp, cache, th, now()); ok {
		t.Errorf("expected no rebalance onto model-capped priority, got %q", pick.Email)
	}

	// Once the model window resets, the rebalance resumes.
	cache["p"] = mw(5, 10, pct(40))
	if pick, ok := ShouldRebalance(KindDrain, []string{"prio@x.test"},
		[]Candidate{priority, temp}, temp, cache, th, now()); !ok || pick.Email != "prio@x.test" {
		t.Errorf("got (%q, %v), want rebalance to prio@x.test", pick.Email, ok)
	}
}

func TestModelCappedReadsEveryReportedModelWindow(t *testing.T) {
	// Each of these is one seat capped on one window. The Opus/Sonnet pair
	// was the whole vocabulary before #52; the rest are the seats that used
	// to read as the healthiest in the pool, get picked, and refuse the very
	// next call — including the Fable window, whose API name shares no
	// prefix with the label Anthropic prints for it.
	for _, window := range []string{
		"seven_day_opus",
		"seven_day_sonnet",
		"seven_day_overage_included",
		"overage",
		"seven_day_some_model_that_does_not_exist_yet",
	} {
		u := mw(5, 10, nil)
		u.Models = map[string]*usage.Window{window: {Utilization: 100}}
		if !modelCapped(usage.Cache{"k": u}, "k") {
			t.Errorf("%s at 100%% must count as model-capped", window)
		}
	}

	// Room in a model window is not a cap, and an unpolled seat is not one
	// either — both must stay eligible as a first pick.
	u := mw(5, 10, nil)
	u.Models = map[string]*usage.Window{"seven_day_opus": {Utilization: 99}}
	if modelCapped(usage.Cache{"k": u}, "k") {
		t.Error("a model window under 100%% must not count as capped")
	}
	if modelCapped(usage.Cache{"k": u}, "missing") {
		t.Error("missing cache entry must not read as capped")
	}
}
