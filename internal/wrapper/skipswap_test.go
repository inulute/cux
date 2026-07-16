package wrapper

import (
	"testing"

	"github.com/inulute/cux/internal/history"
)

func TestSkipSwapOnCapacity(t *testing.T) {
	cases := []struct {
		name           string
		trigger        history.Trigger
		liveKey        string
		rateLimitedKey string
		want           bool
	}{
		{
			// The reported loop: rate-limited, still on that very seat.
			// Its utilisation may read "fine" while the API 429s it, so
			// we must NOT skip (which would resume in place and loop).
			name: "rate-limit, live is still the rate-limited seat", trigger: history.TriggerRateLimit,
			liveKey: "u-a|org", rateLimitedKey: "u-a|org", want: false,
		},
		{
			// The #21 concurrent case: another session already swapped us
			// onto a different, healthy seat — skipping is correct.
			name: "rate-limit, another session already moved us", trigger: history.TriggerRateLimit,
			liveKey: "u-b|org", rateLimitedKey: "u-a|org", want: true,
		},
		{
			name: "rate-limit, unknown rate-limited seat -> never skip", trigger: history.TriggerRateLimit,
			liveKey: "u-a|org", rateLimitedKey: "", want: false,
		},
		{
			name: "rate-limit, unknown live seat -> never skip", trigger: history.TriggerRateLimit,
			liveKey: "", rateLimitedKey: "u-a|org", want: false,
		},
		{
			// Threshold is proactive (no hard limit hit) — capacity is real.
			name: "threshold always allows skip", trigger: history.TriggerThreshold,
			liveKey: "u-a|org", rateLimitedKey: "u-a|org", want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := skipSwapOnCapacity(c.trigger, c.liveKey, c.rateLimitedKey); got != c.want {
				t.Errorf("skipSwapOnCapacity(%q, %q, %q) = %v, want %v",
					c.trigger, c.liveKey, c.rateLimitedKey, got, c.want)
			}
		})
	}
}
