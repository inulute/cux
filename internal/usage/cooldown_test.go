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

// The cap has to stay clear of StaleAfter. A held-back seat keeps serving its
// last reading, and once that reads stale the fail-open rule (#37) treats it
// as having room — so a hold-off long enough to age a reading out would turn
// a rate limit into "this seat is fine", which is how #37 happened.
func TestCooldownCapStaysUnderStaleAfter(t *testing.T) {
	longest := cooldownFor(1 << 20)
	if longest >= StaleAfter {
		t.Fatalf("cooldown caps at %s, StaleAfter is %s: a held seat ages out of trust", longest, StaleAfter)
	}
	if longest > StaleAfter/2 {
		t.Errorf("cooldown cap %s leaves little room under StaleAfter %s", longest, StaleAfter)
	}
}

// The open window set (#52) treats any unknown top-level key that looks like
// a window as a model window. The cooldown fields sit at that same level, so
// they have to stay out of it — and a refusal has to leave a limits[]-derived
// window alone, since the last good reading is the whole point of holding off.
func TestCooldownAndModelWindowsDoNotLeakIntoEachOther(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// A reading that came from limits[], now carrying a hold-off.
	u := AccountUsage{
		PolledAt:   at.Add(-time.Hour),
		SevenDay:   &Window{Utilization: 59},
		Models:     map[string]*Window{"seven_day_fable": {Utilization: 100}},
		RetryAfter: at.Add(time.Minute),
		Failures:   2,
	}
	b, err := u.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back AccountUsage
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{keyRetryAfter, keyFailures, keyLimits} {
		if _, leaked := back.Models[k]; leaked {
			t.Errorf("%q came back as a model window", k)
		}
	}
	if len(back.Models) != 1 || back.Models["seven_day_fable"].Utilization != 100 {
		t.Errorf("model windows = %v, want just seven_day_fable at 100", back.Models)
	}
	if !back.RetryAfter.Equal(u.RetryAfter) || back.Failures != 2 {
		t.Errorf("hold-off lost: %v / %d", back.RetryAfter, back.Failures)
	}
}

// And the other direction: a live response carrying limits[] must not invent
// cooldown state, or a healthy seat would hold itself back.
func TestAFreshResponseCarriesNoHoldOff(t *testing.T) {
	u, err := parseResponse([]byte(`{
	  "seven_day": {"utilization": 59, "resets_at": null},
	  "limits": [{"kind": "weekly_scoped", "percent": 100, "resets_at": null,
	              "scope": {"model": {"display_name": "Fable"}}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !u.RetryAfter.IsZero() || u.Failures != 0 {
		t.Errorf("a successful poll must carry no hold-off: %v / %d", u.RetryAfter, u.Failures)
	}
	if u.Models["seven_day_fable"] == nil {
		t.Error("lost the limits[] window")
	}
}
