package monitor

import (
	"os"
	"testing"
	"time"

	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// seatWithCachedReading sets up an isolated cux home holding one managed
// account whose usage was polled polledAt, and returns its cache key.
func seatWithCachedReading(t *testing.T, polledAt time.Time) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir+"/.local/share")
	t.Setenv("CUX_CREDS_BACKEND", "file")

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
	key := state.Accounts[1].CacheKey()

	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(usage.Cache{key: {PolledAt: polledAt}}); err != nil {
		t.Fatal(err)
	}
	return key
}

// The busy-host case from #53: every wrapped session ends its turns against
// the same seat, so without coalescing this is one API call per session per
// turn against a single account. A sibling's recent reading has to be enough.
func TestRefreshActiveCoalescedSkipsAFetchAnotherSessionAlreadyMade(t *testing.T) {
	polledAt := time.Now().UTC().Add(-5 * time.Second)
	key := seatWithCachedReading(t, polledAt)

	// No credentials exist in this temp home, so any real fetch fails and
	// would either error or overwrite the entry. Neither may happen here.
	if err := RefreshActiveCoalesced("seat@example.com", 20*time.Second); err != nil {
		t.Fatalf("RefreshActiveCoalesced on a fresh reading = %v, want nil", err)
	}

	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	if got := cache[key].PolledAt; !got.Equal(polledAt) {
		t.Errorf("polled_at = %v, want the sibling's reading %v untouched", got, polledAt)
	}
}

// The window has to stay a window. A reading older than maxAge is exactly
// the case the threshold check must not reason from, so it is refetched.
func TestRefreshActiveCoalescedStillFetchesAStaleReading(t *testing.T) {
	seatWithCachedReading(t, time.Now().UTC().Add(-10*time.Minute))

	// The fetch is attempted and fails for want of credentials. The error
	// is the proof it was attempted; a nil return would mean the stale
	// entry had been accepted as current.
	if err := RefreshActiveCoalesced("seat@example.com", 20*time.Second); err == nil {
		t.Error("a reading older than maxAge must be refetched, not reused")
	}
}

// RefreshActive keeps its unconditional contract for callers that need a
// current reading whatever a sibling did (`cux usage refresh`, the paths
// that decide whether a seat is exhausted).
func TestRefreshActiveIgnoresAFreshSiblingReading(t *testing.T) {
	seatWithCachedReading(t, time.Now().UTC())

	if err := RefreshActive("seat@example.com"); err == nil {
		t.Error("RefreshActive must always fetch, even next to a fresh reading")
	}
}
