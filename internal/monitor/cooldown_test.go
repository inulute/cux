package monitor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// seatAgainst429 sets up an isolated home with one managed account whose
// usage endpoint always refuses, and counts how often it is asked.
func seatAgainst429(t *testing.T, retryAfter string) (key string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir+"/.local/share")
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_USAGE_ENDPOINT", srv.URL)

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Add(1, "seat@example.com", "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	key = state.Accounts[1].CacheKey()

	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli())
	if err := creds.WriteBackup(1, "seat@example.com", blob); err != nil {
		t.Fatal(err)
	}
	// A real earlier reading, so the refusal has something to be recorded
	// against — which is the whole point.
	if err := usage.SaveCache(usage.Cache{key: {
		PolledAt: time.Now().Add(-2 * time.Hour),
		FiveHour: &usage.Window{Utilization: 26},
	}}); err != nil {
		t.Fatal(err)
	}
	return key, hits
}

// #53: the seat that fails is the one polled hardest. A failed read leaves
// PolledAt at the last success, so the entry goes on reading stale, and
// staleness is what makes the next caller fetch. Uncoalesced callers then
// hammer the one account that cannot answer — for hours, in the report.
func TestARefusedSeatIsNotPolledAgainImmediately(t *testing.T) {
	key, hits := seatAgainst429(t, "")

	for i := 0; i < 5; i++ {
		_, _ = RefreshAll() // uncoalesced: the caller that used to hammer
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("endpoint asked %d times across 5 uncoalesced refreshes, want 1", got)
	}

	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	u := cache[key]
	if u.Failures != 1 {
		t.Errorf("Failures = %d, want 1", u.Failures)
	}
	if !u.InCooldown(time.Now()) {
		t.Error("want the seat held back after a refusal")
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 26 {
		t.Errorf("the last good reading must survive: %+v", u.FiveHour)
	}
	if u.PolledAt.After(time.Now().Add(-time.Hour)) {
		t.Error("a refusal must not pass itself off as a fresh poll")
	}
}

// A Retry-After the endpoint names is honoured over the backoff.
func TestRetryAfterHeaderSetsTheHoldOff(t *testing.T) {
	key, _ := seatAgainst429(t, "1800")
	_, _ = RefreshAll()

	cache, _ := usage.LoadCache()
	got := time.Until(cache[key].RetryAfter)
	if got < 29*time.Minute || got > 30*time.Minute+time.Second {
		t.Errorf("hold-off = %v, want the named ~30m", got)
	}
}
