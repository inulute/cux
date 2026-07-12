// Package usage talks to the Anthropic usage API
// (https://api.anthropic.com/api/oauth/usage) so cux can show users
// how much of their 5-hour and 7-day budgets each managed account has
// consumed, and so the wrapper can swap accounts before a hard cap is
// hit.
//
// The API requires an OAuth bearer token from a Pro/Max subscription
// account and the beta header `anthropic-beta: oauth-2025-04-20`
// (verified live on 2026-05-01). Responses include a small set of
// always-present windows (five_hour, seven_day) plus model- and
// program-specific windows that may appear or disappear over time;
// we tolerate unknown fields silently rather than fail on them.
//
// All on-disk state goes through `atomicfile` at mode 0600.
package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// API endpoint and beta header. Both can be overridden via env vars
// for testing or migration to a future header value, so we don't have
// to ship a new binary if Anthropic rolls the beta tag.
const (
	defaultEndpoint = "https://api.anthropic.com/api/oauth/usage"
	defaultBetaHdr  = "oauth-2025-04-20"
	envEndpoint     = "CUX_USAGE_ENDPOINT"
	envBetaHeader   = "CUX_USAGE_BETA"
	cacheFileName   = "usage-cache.json"
	httpTimeout     = 10 * time.Second
)

// Window is one usage interval reported by the API.
type Window struct {
	Utilization float64    `json:"utilization"`         // 0.0–100.0
	ResetsAt    *time.Time `json:"resets_at,omitempty"` // nil if API returned null
}

// AccountUsage is one account's snapshot of every window we care
// about. PolledAt records when this snapshot was captured;
// TokenExpired is set when fetching with the account's stored token
// returned 401 (so the user knows to `claude login` and `cux add`
// again).
type AccountUsage struct {
	FiveHour       *Window   `json:"five_hour,omitempty"`
	SevenDay       *Window   `json:"seven_day,omitempty"`
	SevenDaySonnet *Window   `json:"seven_day_sonnet,omitempty"`
	SevenDayOpus   *Window   `json:"seven_day_opus,omitempty"`
	PolledAt       time.Time `json:"polled_at"`
	TokenExpired   bool      `json:"token_expired,omitempty"`
}

// Cache is the on-disk usage cache, keyed by account email.
type Cache map[string]AccountUsage

// Thresholds are integer percentages 0–100. A threshold of 100 means
// "reactive only" — never preemptively swap on this window.
type Thresholds struct {
	FiveHour int `json:"five_hour"`
	SevenDay int `json:"seven_day"`
}

// Default thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{FiveHour: 100, SevenDay: 100}
}

// Fetch hits the usage API with the given OAuth access token and
// returns the parsed response as an AccountUsage. Network failures,
// non-200 statuses and parse errors all surface as errors; the caller
// decides whether to mark the cached entry stale.
//
// 401 responses are detected and surfaced as ErrTokenExpired so the
// caller can mark the account for re-login.
func Fetch(token string) (AccountUsage, error) {
	if token == "" {
		return AccountUsage{}, fmt.Errorf("usage: empty access token")
	}
	endpoint := os.Getenv(envEndpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	beta := os.Getenv(envBetaHeader)
	if beta == "" {
		beta = defaultBetaHdr
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return AccountUsage{}, fmt.Errorf("usage: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", beta)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return AccountUsage{}, fmt.Errorf("usage: HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return AccountUsage{}, fmt.Errorf("usage: read body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return AccountUsage{TokenExpired: true, PolledAt: time.Now().UTC()}, ErrTokenExpired
	}
	if resp.StatusCode != http.StatusOK {
		return AccountUsage{}, fmt.Errorf("usage: HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	var raw apiResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return AccountUsage{}, fmt.Errorf("usage: parse: %w (body: %s)", err, snippet(body))
	}
	return AccountUsage{
		FiveHour:       raw.FiveHour,
		SevenDay:       raw.SevenDay,
		SevenDaySonnet: raw.SevenDaySonnet,
		SevenDayOpus:   raw.SevenDayOpus,
		PolledAt:       time.Now().UTC(),
	}, nil
}

// ErrTokenExpired is returned by Fetch when the API rejects the token
// with 401. The returned AccountUsage has TokenExpired = true so the
// caller can write it through to the cache without losing the signal.
var ErrTokenExpired = fmt.Errorf("usage: token expired (re-login and `cux add`)")

// LoadCache reads the on-disk usage cache. A missing file yields an
// empty cache, not an error — fresh installs are normal.
func LoadCache() (Cache, error) {
	b, err := os.ReadFile(cachePath())
	if err != nil {
		if os.IsNotExist(err) {
			return Cache{}, nil
		}
		return nil, fmt.Errorf("usage: read cache: %w", err)
	}
	if len(b) == 0 {
		return Cache{}, nil
	}
	c := Cache{}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("usage: parse cache: %w", err)
	}
	return c, nil
}

// SaveCache writes the usage cache atomically at mode 0600.
func SaveCache(c Cache) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return fmt.Errorf("usage: mkdir runtime: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("usage: marshal cache: %w", err)
	}
	return atomicfile.Write(cachePath(), data, 0o600)
}

// IsOverThreshold reports whether u has crossed either configured
// threshold. Returns the human-readable reason if so; the caller logs
// it into the swap history.
//
// A threshold of 100 means "reactive only" — never trigger preemptively
// on that window. However, when utilization is genuinely at the hard limit
// (100%), the account is blocked regardless of threshold preference, so we
// always return true. This ensures the session-limit case (where Claude Code
// blocks at the UI layer before any tool use, so PostToolUseFailure never
// fires) is still caught by the prompt-submit and stop-signal paths.
//
// We treat a missing window (nil pointer) as "no data, no decision" — never
// as "definitely under threshold."
func IsOverThreshold(u AccountUsage, t Thresholds) (over bool, reason string) {
	// Hard-limit check: genuinely exhausted accounts must always trigger a
	// switch regardless of the configured threshold.
	if u.FiveHour != nil && u.FiveHour.Utilization >= 100 {
		return true, "5h utilization at hard limit (100%)"
	}
	if u.SevenDay != nil && u.SevenDay.Utilization >= 100 {
		return true, "7d utilization at hard limit (100%)"
	}
	if t.SevenDay > 0 && t.SevenDay < 100 && u.SevenDay != nil {
		if u.SevenDay.Utilization >= float64(t.SevenDay) {
			return true, fmt.Sprintf("7d utilization %.0f%% ≥ threshold %d%%",
				u.SevenDay.Utilization, t.SevenDay)
		}
	}
	if t.FiveHour > 0 && t.FiveHour < 100 && u.FiveHour != nil {
		if u.FiveHour.Utilization >= float64(t.FiveHour) {
			return true, fmt.Sprintf("5h utilization %.0f%% ≥ threshold %d%%",
				u.FiveHour.Utilization, t.FiveHour)
		}
	}
	return false, ""
}

// --- internals -------------------------------------------------------------

// apiResponse mirrors the documented (and observed) shape of the
// usage endpoint. Extra fields not modelled here are silently dropped
// by the JSON decoder, which is the right behavior — the API has
// added several non-standard windows over time and may add more.
type apiResponse struct {
	FiveHour       *Window `json:"five_hour"`
	SevenDay       *Window `json:"seven_day"`
	SevenDaySonnet *Window `json:"seven_day_sonnet"`
	SevenDayOpus   *Window `json:"seven_day_opus"`
}

func cachePath() string {
	return filepath.Join(paths.RuntimeDir(), cacheFileName)
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}
