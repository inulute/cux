package strategy

import (
	"testing"

	"github.com/inulute/cux/internal/usage"
)

// Twin seats: the same email holding a personal subscription and an
// organization seat. Emails cannot distinguish them — seat keys can.
func twinFixture() ([]Candidate, Candidate, usage.Cache) {
	personal := Candidate{Email: "me@x.test", Slot: 1, CacheKey: "u-me|org-personal"}
	orgSeat := Candidate{Email: "me@x.test", Slot: 2, CacheKey: "u-me|org-corp"}
	other := Candidate{Email: "other@x.test", Slot: 3, CacheKey: "u-other|org-corp"}

	cache := usage.Cache{
		"u-me|org-personal": {FiveHour: &usage.Window{Utilization: 100}, SevenDay: &usage.Window{Utilization: 60}},
		"u-me|org-corp":     {FiveHour: &usage.Window{Utilization: 5}, SevenDay: &usage.Window{Utilization: 10}},
		"u-other|org-corp":  {FiveHour: &usage.Window{Utilization: 100}, SevenDay: &usage.Window{Utilization: 100}},
	}
	return []Candidate{personal, orgSeat, other}, personal, cache
}

func TestPickNextDistinguishesTwinSeats(t *testing.T) {
	accounts, current, cache := twinFixture()
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}

	// Exhausted personal seat must be able to rotate onto the SAME
	// email's healthy org seat. With email-keyed comparison both twins
	// were excluded and the pick failed (or fell through to the also
	// exhausted third account).
	pick, ok := PickNext(KindDrain, nil, accounts, current, cache, th)
	if !ok {
		t.Fatal("expected a pick, got none — twin seat was excluded along with current")
	}
	if pick.Slot != 2 {
		t.Errorf("picked slot %d (%s), want the twin org seat (slot 2)", pick.Slot, pick.Reason)
	}
	if pick.Identifier() != "2" {
		t.Errorf("Identifier() = %q, want the unambiguous slot number \"2\"", pick.Identifier())
	}
}

func TestPickBalancedDistinguishesTwinSeats(t *testing.T) {
	accounts, current, cache := twinFixture()
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}

	pick, ok := PickNext(KindBalanced, nil, accounts, current, cache, th)
	if !ok || pick.Slot != 2 {
		t.Errorf("got (slot %d, %v), want the twin org seat (slot 2, true)", pick.Slot, ok)
	}
}

func TestOrderedTwinSeatsBothConsidered(t *testing.T) {
	accounts, current, cache := twinFixture()
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}

	// The priority list can only speak in emails; both seats behind an
	// email must be considered, minus the current one.
	pick, ok := PickNext(KindDrain, []string{"me@x.test", "other@x.test"}, accounts, current, cache, th)
	if !ok || pick.Slot != 2 {
		t.Errorf("got (slot %d, %v), want the twin org seat (slot 2, true)", pick.Slot, ok)
	}
}

func TestIdentifierFallsBackToEmail(t *testing.T) {
	p := Pick{Email: "legacy@x.test"}
	if p.Identifier() != "legacy@x.test" {
		t.Errorf("Identifier() = %q, want email fallback for slotless picks", p.Identifier())
	}
}
