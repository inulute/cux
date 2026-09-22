package usage

import (
	"net/http"
	"testing"
	"time"
)

// The endpoint's own answer wins when it gives one; otherwise the backoff
// decides. Either way a refused account has to be held back, or the reading
// stays stale and staleness is what makes the next caller poll it (#53).
func TestNextRetry(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// Consecutive failures back off, and stop growing at the cap.
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{5, 15 * time.Minute},
		{50, 15 * time.Minute},
	} {
		u := AccountUsage{Failures: tc.failures}
		if got := u.NextRetry(now, 0).Sub(now); got != tc.want {
			t.Errorf("after %d failures: %v, want %v", tc.failures, got, tc.want)
		}
	}

	// A longer Retry-After is honoured; a shorter one does not shorten the
	// backoff, or a server saying "1s" would undo it.
	u := AccountUsage{Failures: 1}
	if got := u.NextRetry(now, time.Hour).Sub(now); got != time.Hour {
		t.Errorf("named Retry-After ignored: %v", got)
	}
	if got := u.NextRetry(now, time.Second).Sub(now); got != time.Minute {
		t.Errorf("a short Retry-After shortened the backoff: %v", got)
	}
}

func TestInCooldown(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if (AccountUsage{}).InCooldown(now) {
		t.Error("an account that never failed is not in cooldown")
	}
	if !(AccountUsage{RetryAfter: now.Add(time.Minute)}).InCooldown(now) {
		t.Error("want cooldown while RetryAfter is ahead")
	}
	if (AccountUsage{RetryAfter: now.Add(-time.Second)}).InCooldown(now) {
		t.Error("cooldown must end once RetryAfter has passed")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("120"); got != 2*time.Minute {
		t.Errorf("seconds form: %v", got)
	}
	future := time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 80*time.Second || got > 90*time.Second {
		t.Errorf("http-date form: %v", got)
	}
	for _, in := range []string{"", "   ", "soon", "0", "-5",
		time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)} {
		if got := parseRetryAfter(in); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0 so the backoff decides", in, got)
		}
	}
}

// The cooldown has to survive the cache file: N sessions on a host back off
// together only if they all read the same hold-off.
func TestCooldownRoundTripsThroughTheCache(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 30, 0, 0, time.UTC)
	in := AccountUsage{
		PolledAt:   at.Add(-2 * time.Hour),
		FiveHour:   &Window{Utilization: 26},
		RetryAfter: at,
		Failures:   3,
	}
	b, err := in.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out AccountUsage
	if err := out.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if !out.RetryAfter.Equal(at) || out.Failures != 3 {
		t.Fatalf("round trip lost the hold-off: %+v", out)
	}
	if out.FiveHour == nil || out.FiveHour.Utilization != 26 {
		t.Errorf("the last good reading must survive a refusal: %+v", out.FiveHour)
	}
	if _, isWindow := out.Models[keyRetryAfter]; isWindow {
		t.Error("retry_after must not be read back as a model window")
	}
}
