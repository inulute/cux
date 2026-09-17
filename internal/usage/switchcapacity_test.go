package usage

import (
	"testing"
	"time"
)

// The /switch precheck in the hook and the wrapper's target resolution have
// to answer this identically. When they did not, the hook accepted a switch
// the wrapper could not perform and claude was relaunched on the same seat.
func TestHasSwitchCapacity(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	th := Thresholds{FiveHour: 90}
	at := func(pct float64, resets time.Time) *Window {
		return &Window{Utilization: pct, ResetsAt: &resets}
	}

	cases := []struct {
		name string
		u    AccountUsage
		want bool
	}{
		{"under the threshold", AccountUsage{PolledAt: now, FiveHour: at(40, now.Add(time.Hour))}, true},
		{"over the threshold", AccountUsage{PolledAt: now, FiveHour: at(95, now.Add(time.Hour))}, false},
		{"seven-day capped", AccountUsage{PolledAt: now, SevenDay: at(100, now.Add(48*time.Hour))}, false},
		{"token expired", AccountUsage{PolledAt: now, TokenExpired: true}, false},
		{
			// The window's reset has passed, so the reading is spent even
			// though the API has not rolled it over yet.
			"five-hour window already reset",
			AccountUsage{PolledAt: now, FiveHour: at(100, now.Add(-time.Minute))},
			true,
		},
		{
			// #37: a reading nobody can vouch for is not evidence of
			// exhaustion. Refusing on one strands a live session.
			"stale reading reads as room",
			AccountUsage{PolledAt: now.Add(-72 * time.Hour), FiveHour: at(100, now.Add(time.Hour))},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasSwitchCapacity(tc.u, th, now); got != tc.want {
				t.Errorf("HasSwitchCapacity = %v, want %v", got, tc.want)
			}
		})
	}
}
