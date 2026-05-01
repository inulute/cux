// Package updater is the opt-in update-check path. It is never invoked
// unless config.UpdateCheck.Enabled is true; cux makes no network call
// out of the box.
//
// The check hits the GitHub Releases API for the inulute/cux
// repo, compares the latest tag to the running binary's version, and
// returns either nothing or a Result describing the newer version.
// Results are cached on disk so we ping GitHub at most once per the
// configured cadence regardless of how often the user runs cux.
package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

const (
	defaultRepo    = "inulute/cux"
	envRepo        = "CUX_RELEASE_REPO"
	releasesAPIFmt = "https://api.github.com/repos/%s/releases/latest"
	cacheFileName  = "update-cache.json"
	httpTimeout    = 5 * time.Second
)

// Result describes a newer release. Empty Latest means "no update".
type Result struct {
	Current string    // version cux was built as, no leading "v"
	Latest  string    // GitHub tag minus leading "v"
	HTMLURL string    // browser-friendly link to the release
	Polled  time.Time // when we last hit the API
}

// HasUpdate is true when Latest is set and semantically newer than Current.
func (r Result) HasUpdate() bool {
	return r.Latest != "" && IsNewer(stripV(r.Latest), stripV(r.Current))
}

// Cache is the on-disk shape persisted between runs.
type Cache struct {
	Polled  time.Time `json:"polled"`
	Latest  string    `json:"latest"`
	HTMLURL string    `json:"htmlUrl"`
}

// CachedCheck returns a Result for `current` using a recent cache when
// available, otherwise a fresh API call. The bool return is true iff a
// fresh API call was actually made (so callers can avoid double-fetching
// from a goroutine + a defer).
//
// Errors are deliberately conservative: a network blip yields the cached
// value (if any) rather than alarming the user. Only outright cache
// corruption surfaces as an error.
func CachedCheck(current string, cadence time.Duration) (Result, bool, error) {
	cache, _ := loadCache()
	if cache != nil && time.Since(cache.Polled) < cadence {
		return Result{
			Current: stripV(current),
			Latest:  stripV(cache.Latest),
			HTMLURL: cache.HTMLURL,
			Polled:  cache.Polled,
		}, false, nil
	}
	r, err := fetchLatest(current)
	if err != nil {
		// Stale cache is better than no cache: surface what we have.
		if cache != nil {
			return Result{
				Current: stripV(current),
				Latest:  stripV(cache.Latest),
				HTMLURL: cache.HTMLURL,
				Polled:  cache.Polled,
			}, false, nil
		}
		return Result{}, false, err
	}
	_ = saveCache(Cache{Polled: r.Polled, Latest: r.Latest, HTMLURL: r.HTMLURL})
	return r, true, nil
}

// fetchLatest makes the HTTP call. Exported only via CachedCheck so the
// cache discipline can't be bypassed accidentally.
func fetchLatest(current string) (Result, error) {
	repo := os.Getenv(envRepo)
	if repo == "" {
		repo = defaultRepo
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(releasesAPIFmt, repo), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cux/"+stripV(current))

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("updater: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return Result{}, err
	}
	var raw struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Result{}, err
	}
	return Result{
		Current: stripV(current),
		Latest:  stripV(raw.TagName),
		HTMLURL: raw.HTMLURL,
		Polled:  time.Now().UTC(),
	}, nil
}

func cachePath() string {
	return filepath.Join(paths.RuntimeDir(), cacheFileName)
}

func loadCache() (*Cache, error) {
	b, err := os.ReadFile(cachePath())
	if err != nil {
		return nil, err
	}
	var c Cache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveCache(c Cache) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(cachePath(), data, 0o600)
}

func stripV(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		return s[1:]
	}
	return s
}

// IsNewer reports whether a is a newer semver than b. Pre-release and
// build metadata are ignored — sufficient for cux's release policy
// (we ship plain MAJOR.MINOR.PATCH tags). Bad input falls back to
// string comparison.
func IsNewer(a, b string) bool {
	an, aOK := splitNums(a)
	bn, bOK := splitNums(b)
	if !aOK || !bOK {
		return strings.Compare(a, b) > 0
	}
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			return an[i] > bn[i]
		}
	}
	return false
}

func splitNums(s string) ([3]int, bool) {
	// Drop any pre-release / build suffix (everything after the first '-' or '+')
	for i, r := range s {
		if r == '-' || r == '+' {
			s = s[:i]
			break
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
