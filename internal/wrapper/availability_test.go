package wrapper

import (
	"testing"
	"time"

	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

func win(util float64, resetsAt *time.Time) *usage.Window {
	return &usage.Window{Utilization: util, ResetsAt: resetsAt}
}

func TestNextAvailability(t *testing.T) {
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	in1h := now.Add(1 * time.Hour)
	in2h := now.Add(2 * time.Hour)
	in3d := now.Add(72 * time.Hour)
	th := usage.Thresholds{FiveHour: 100, SevenDay: 100}

	accounts := map[int]store.Account{
		1: {Slot: 1, Email: "a@x.test"},
		2: {Slot: 2, Email: "b@x.test"},
	}

	t.Run("picks the earliest 5h reset", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(100, &in2h), SevenDay: win(50, &in3d)},
			"b@x.test": {FiveHour: win(100, &in1h), SevenDay: win(40, &in3d)},
		}
		at, email, ok := nextAvailability(accounts, cache, th, now)
		if !ok || email != "b@x.test" || !at.Equal(in1h) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, in1h, "b@x.test")
		}
	})

	t.Run("capped 7d window binds over an earlier 5h reset", func(t *testing.T) {
		cache := usage.Cache{
			// a: 5h resets first, but its 7d window is also full until in3d.
			"a@x.test": {FiveHour: win(100, &in1h), SevenDay: win(100, &in3d)},
			"b@x.test": {FiveHour: win(100, &in2h), SevenDay: win(40, &in3d)},
		}
		at, email, ok := nextAvailability(accounts, cache, th, now)
		if !ok || email != "b@x.test" || !at.Equal(in2h) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, in2h, "b@x.test")
		}
	})

	t.Run("skips windows missing reset stamps and expired tokens", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(100, nil)},
			"b@x.test": {FiveHour: win(100, &in1h), TokenExpired: true},
		}
		if _, _, ok := nextAvailability(accounts, cache, th, now); ok {
			t.Error("expected ok=false when no account has a usable reset time")
		}
	})

	t.Run("account under threshold is ready now", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(100, &in2h)},
			"b@x.test": {FiveHour: win(30, &in1h)},
		}
		at, email, ok := nextAvailability(accounts, cache, th, now)
		if !ok || email != "b@x.test" || !at.Equal(now) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, now, "b@x.test")
		}
	})

	t.Run("respects lowered thresholds", func(t *testing.T) {
		lowered := usage.Thresholds{FiveHour: 90, SevenDay: 100}
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(95, &in1h)},
			"b@x.test": {FiveHour: win(92, &in2h)},
		}
		at, email, ok := nextAvailability(accounts, cache, lowered, now)
		if !ok || email != "a@x.test" || !at.Equal(in1h) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, in1h, "a@x.test")
		}
	})
}
