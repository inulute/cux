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

	t.Run("under-threshold account is never named as earliest reset", func(t *testing.T) {
		// a is exhausted (reset in2h); b has plenty of room. b must not be
		// reported as "ready now" — it has capacity, so there is nothing to
		// wait for, and naming it produces the bogus sub-slack countdown of
		// issue #37. The genuinely-capped account's reset is what a wait
		// should target.
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(100, &in2h)},
			"b@x.test": {FiveHour: win(30, &in1h)},
		}
		at, email, ok := nextAvailability(accounts, cache, th, now)
		if !ok || email != "a@x.test" || !at.Equal(in2h) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, in2h, "a@x.test")
		}
	})

	t.Run("all accounts under threshold → nothing to wait for", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {FiveHour: win(20, &in1h), SevenDay: win(7, &in3d)},
			"b@x.test": {FiveHour: win(30, &in2h), SevenDay: win(9, &in3d)},
		}
		if at, email, ok := nextAvailability(accounts, cache, th, now); ok {
			t.Errorf("got (%v, %q, true), want ok=false — no account is over threshold", at, email)
		}
	})

	t.Run("a capped window whose reset is already past is skipped", func(t *testing.T) {
		past := now.Add(-5 * time.Minute)
		cache := usage.Cache{
			// a's 5h window is over the cap but its stamped reset already
			// elapsed (cache lag after a roll-over) — treat as unknown and
			// re-poll rather than returning a sub-slack now-ish deadline.
			"a@x.test": {FiveHour: win(100, &past)},
			"b@x.test": {FiveHour: win(100, &in2h)},
		}
		at, email, ok := nextAvailability(accounts, cache, th, now)
		if !ok || email != "b@x.test" || !at.Equal(in2h) {
			t.Errorf("got (%v, %q, %v), want (%v, %q, true)", at, email, ok, in2h, "b@x.test")
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
