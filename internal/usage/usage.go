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
	"strconv"
	"strings"
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
	FiveHour *Window
	SevenDay *Window
	// Models holds every other usage window the endpoint reports: top-level
	// windows under the API's own name, limits[] model scopes under
	// seven_day_<family> (`seven_day_fable`).
	//
	// Open rather than a fixed set of fields, because the set is not ours to
	// fix. Anthropic exposes several model- and program-specific limits and
	// has added to them over time; a closed pair meant a seat capped on any
	// newer one parsed into nothing, so every window cux consulted showed
	// room and the seat ranked as the healthiest in the pool. It was then
	// picked, refused the next call, and swapped straight back — a four
	// second round trip, and a second seat spent for nothing (#52).
	//
	// A new model family therefore needs no cux release; whatever the
	// endpoint names, cux ranks on it.
	Models       map[string]*Window
	PolledAt     time.Time
	TokenExpired bool
	// RetryAfter holds off the next poll of this account. A failed read
	// leaves PolledAt where the last success put it, so the reading goes on
	// looking stale — and staleness is what makes the next caller fetch. The
	// endpoint then 429s the one account every session shares while the idle
	// ones coalesce normally, which is #53: one seat frozen for hours, polled
	// every time precisely because its last poll failed.
	//
	// Stored in the shared cache rather than per process, so N wrappers on a
	// host back off together instead of each discovering the limit alone.
	RetryAfter time.Time
	// Failures counts consecutive failed polls; it sets how far RetryAfter
	// moves and is cleared by the first success.
	Failures int
}

// InCooldown reports whether this account is being held back from polling.
func (u AccountUsage) InCooldown(now time.Time) bool {
	return !u.RetryAfter.IsZero() && now.Before(u.RetryAfter)
}

// cooldownFor is the hold-off after n consecutive failures: a minute,
// doubling, capped. Long enough that a host full of sessions stops adding
// load, short enough that a transient refusal costs one reading.
func cooldownFor(n int) time.Duration {
	const base, cap = time.Minute, 15 * time.Minute
	d := base
	for i := 1; i < n && d < cap; i++ {
		d *= 2
	}
	return min(d, cap)
}

// NextRetry returns when a failed poll may be tried again. A Retry-After the
// endpoint named is honoured as a floor — it knows better than the backoff.
func (u AccountUsage) NextRetry(now time.Time, named time.Duration) time.Time {
	d := cooldownFor(u.Failures)
	if named > d {
		d = named
	}
	return now.Add(d)
}

// Reserved top-level keys in the cache and API shapes: everything that is
// not one of these, and looks like a window, is a model window.
const (
	keyFiveHour     = "five_hour"
	keySevenDay     = "seven_day"
	keyPolledAt     = "polled_at"
	keyTokenExpired = "token_expired"
	keyLimits       = "limits"
	keyRetryAfter   = "retry_after"
	keyFailures     = "failures"
)

// limitEntry is one element of the endpoint's limits[] array. Model-scoped
// weekly caps are reported only here; the top-level seven_day_opus and
// seven_day_sonnet keys are null on current accounts.
type limitEntry struct {
	Kind     string     `json:"kind"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// KnownModels are the model families Anthropic names in its own limit
// labels ("You've reached your Fable limit"). They live here because two
// surfaces read them: the rejection text the wrapper parses, and the display
// names this endpoint reports in limits[]. A name matching neither still
// works — it just keys a window of its own.
var KnownModels = []string{"opus", "sonnet", "haiku", "fable"}

// modelWindowKey names the window for a model scope from limits[].
//
// Display names carry a version and the version moves — "Fable" today,
// "Claude Fable 5.1" the moment Anthropic spells it out — so the raw name is
// not a key. Anything outside [a-z0-9] becomes an underscore, then a
// recognised family wins: "Claude Fable 5.1" and "Fable" both key
// seven_day_fable, which is also what the top-level key was called when the
// endpoint still sent one. An unrecognised name keeps its slug, so a family
// cux has never heard of still gets ranked (#52).
func modelWindowKey(displayName string) string {
	slug := slugify(displayName)
	if slug == "" {
		return ""
	}
	for _, part := range strings.Split(slug, "_") {
		for _, m := range KnownModels {
			if part == m {
				return keySevenDay + "_" + m
			}
		}
	}
	return keySevenDay + "_" + slug
}

func slugify(s string) string {
	var b strings.Builder
	lastUnderscore := true // also trims a leading underscore
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// decodeLimits turns each weekly_scoped entry naming a model into a window
// keyed seven_day_<model>, so it sits beside seven_day in the cache. Entries
// without a model scope describe the seat as a whole, which five_hour and
// seven_day already cover.
func decodeLimits(v json.RawMessage) map[string]*Window {
	var entries []limitEntry
	if err := json.Unmarshal(v, &entries); err != nil {
		return nil
	}
	out := map[string]*Window{}
	for _, e := range entries {
		if e.Kind != "weekly_scoped" || e.Percent == nil || e.Scope == nil || e.Scope.Model == nil {
			continue
		}
		name := modelWindowKey(e.Scope.Model.DisplayName)
		if name == "" || reservedKey(name) {
			continue
		}
		out[name] = &Window{Utilization: *e.Percent, ResetsAt: e.ResetsAt}
	}
	return out
}

// AccountUsage is stored flat — `seven_day_opus` sits beside `five_hour` at
// the top level rather than under a nested `models` object — so the on-disk
// format is unchanged by the move to an open set. A cache written here still
// reads correctly in a build that knew only the fixed pair, which matters
// because a user can downgrade cux without being asked to discard state.
func (u AccountUsage) MarshalJSON() ([]byte, error) {
	out := map[string]any{keyPolledAt: u.PolledAt}
	if u.FiveHour != nil {
		out[keyFiveHour] = u.FiveHour
	}
	if u.SevenDay != nil {
		out[keySevenDay] = u.SevenDay
	}
	if u.TokenExpired {
		out[keyTokenExpired] = true
	}
	if !u.RetryAfter.IsZero() {
		out[keyRetryAfter] = u.RetryAfter
	}
	if u.Failures > 0 {
		out[keyFailures] = u.Failures
	}
	for name, w := range u.Models {
		if w == nil || reservedKey(name) {
			continue
		}
		out[name] = w
	}
	return json.Marshal(out)
}

func (u *AccountUsage) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*u = AccountUsage{}
	var limits json.RawMessage
	for name, v := range raw {
		switch name {
		case keyPolledAt:
			if err := json.Unmarshal(v, &u.PolledAt); err != nil {
				return err
			}
		case keyTokenExpired:
			if err := json.Unmarshal(v, &u.TokenExpired); err != nil {
				return err
			}
		case keyRetryAfter:
			_ = json.Unmarshal(v, &u.RetryAfter)
		case keyFailures:
			_ = json.Unmarshal(v, &u.Failures)
		case keyFiveHour:
			u.FiveHour = decodeWindow(v)
		case keySevenDay:
			u.SevenDay = decodeWindow(v)
		case keyLimits:
			// Applied after the loop: a limits[] entry and a top-level key can
			// name the same window, and `range raw` is map order, so deciding
			// it here would pick a different winner from run to run.
			limits = v
		default:
			// A window at 0% with no reset carries no signal: that is how the
			// endpoint reports program windows that do not apply to the seat.
			if w := decodeWindow(v); w != nil && (w.Utilization != 0 || w.ResetsAt != nil) {
				u.setModel(name, w)
			}
		}
	}
	// limits[] is where the endpoint reports model scopes now, so it wins.
	for name, w := range decodeLimits(limits) {
		u.setModel(name, w)
	}
	return nil
}

func reservedKey(name string) bool {
	switch name {
	case keyFiveHour, keySevenDay, keyPolledAt, keyTokenExpired, keyLimits, keyRetryAfter, keyFailures:
		return true
	}
	return false
}

func (u *AccountUsage) setModel(name string, w *Window) {
	if w == nil {
		return
	}
	if u.Models == nil {
		u.Models = map[string]*Window{}
	}
	u.Models[name] = w
}

// decodeWindow accepts a value only if it is shaped like a window — an
// object carrying a numeric `utilization`.
//
// Matched on shape rather than on a name prefix deliberately. The limit
// types Anthropic names in its own rejection text do not share one prefix
// (`overage` sits outside the `seven_day_*` family, and the rejection's Fable
// type is `seven_day_overage_included`, a key this endpoint never sends —
// Fable arrives in limits[]), so a name-based guess catches neither. Shape
// matches the package's promise to tolerate unknown fields.
func decodeWindow(v json.RawMessage) *Window {
	var probe struct {
		Utilization *float64 `json:"utilization"`
	}
	if err := json.Unmarshal(v, &probe); err != nil || probe.Utilization == nil {
		return nil
	}
	var w Window
	if err := json.Unmarshal(v, &w); err != nil {
		return nil
	}
	return &w
}

// Cache is the on-disk usage cache, keyed by account email.
type Cache map[string]AccountUsage

// StaleAfter is how long a cached reading may stand in for a live one.
// Past it the entry records what was true once, not what is true now, and
// every surface that shows it must say so.
//
// A failed poll leaves the previous entry in place — deliberately, so a
// network blip doesn't erase the pool's last known state — which means age
// is the only evidence that a reading has stopped tracking reality. When
// credentials become unreadable, nothing else distinguishes a twelve-day-old
// number from one fetched a second ago (issue #46).
//
// The bound has to clear the wrapper's coalescing windows (20 s between
// sibling sessions, 2 min for idle ones) by a wide margin. Those windows
// exist so a burst of sessions collapses into a single API sweep instead of
// getting rate-limited (issue #39); a bound anywhere near them would mark
// the whole pool unknown during exactly the load they were built to absorb.
const StaleAfter = 30 * time.Minute

// Age reports how long ago u was polled. ok is false for an entry that was
// never polled, which has no age rather than an age of zero.
func (u AccountUsage) Age(now time.Time) (age time.Duration, ok bool) {
	if u.PolledAt.IsZero() {
		return 0, false
	}
	return now.Sub(u.PolledAt), true
}

// StaleReading reports whether u is old enough to mislead: it was polled,
// and that poll is now too far in the past to stand for the present.
//
// Deliberately not the negation of "fresh". An entry carrying no polled_at
// has an *unknown* age, not a proven stale one — cache files written before
// the field existed look like that — and relabelling those as stale would
// declare a whole pool untrustworthy on no evidence. Surfaces already have
// wording for "no usage data"; that case belongs to them.
func (u AccountUsage) StaleReading(now time.Time) bool {
	age, ok := u.Age(now)
	return ok && age >= StaleAfter
}

// Staleness summarises how much of the cache has stopped tracking reality.
// It is what the banner line is built from.
type Staleness struct {
	Stale  int           // entries present but older than StaleAfter
	Total  int           // entries present for the requested keys
	Oldest time.Duration // age of the oldest stale entry
}

// Any reports whether at least one reading is too old to act on.
func (s Staleness) Any() bool { return s.Stale > 0 }

// All reports whether every reading the pool has is too old to act on.
func (s Staleness) All() bool { return s.Total > 0 && s.Stale == s.Total }

// Staleness measures the entries stored under cacheKeys. Only entries that
// carry a poll time are counted, so Total is the number of readings whose
// age is knowable and Oldest is always a real measured age.
func (c Cache) Staleness(cacheKeys []string, now time.Time) Staleness {
	var out Staleness
	for _, k := range cacheKeys {
		u, ok := c[k]
		if !ok {
			continue
		}
		age, dated := u.Age(now)
		if !dated {
			continue
		}
		out.Total++
		if age < StaleAfter {
			continue
		}
		out.Stale++
		if age > out.Oldest {
			out.Oldest = age
		}
	}
	return out
}

// HumanAge renders a duration the way the status surfaces do: coarse, and
// never more precise than the number deserves.
func HumanAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f d", d.Hours()/24)
	case d >= 2*time.Hour:
		return fmt.Sprintf("%.0f h", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f min", d.Minutes())
	default:
		return "under a minute"
	}
}

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
	if resp.StatusCode == http.StatusTooManyRequests {
		return AccountUsage{}, &RateLimitedError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       snippet(body),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return AccountUsage{}, fmt.Errorf("usage: HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	u, err := parseResponse(body)
	if err != nil {
		return AccountUsage{}, fmt.Errorf("usage: parse: %w (body: %s)", err, snippet(body))
	}
	u.PolledAt = time.Now().UTC()
	return u, nil
}

// ErrTokenExpired is returned by Fetch when the API rejects the token
// with 401. The returned AccountUsage has TokenExpired = true so the
// caller can write it through to the cache without losing the signal.
var ErrTokenExpired = fmt.Errorf("usage: token expired (re-login and `cux add`)")

// RateLimitedError is the endpoint refusing a poll. It is carried as its own
// type because the caller has to treat it differently from a failure: a
// failed read that leaves the cached reading untouched looks stale, and a
// stale reading is exactly what makes the next caller poll again (#53).
type RateLimitedError struct {
	RetryAfter time.Duration // 0 when the response named none
	Body       string
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("usage: rate limited by the endpoint, retry after %s", e.RetryAfter)
	}
	return "usage: rate limited by the endpoint"
}

// parseRetryAfter reads the header in either form the RFC allows: seconds, or
// an HTTP date. Anything else is no answer, and the caller picks its own.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

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

// Settled returns u with every window whose reset instant has already
// passed cleared to nil. That window has rolled over, so the utilization
// recorded against it describes a period that is over, not the budget the
// account has now.
//
// nil rather than 0 on purpose: nil is the "unknown" every consumer here
// already handles — IsOverThreshold skips it, usageHasPromptCapacity reads
// it as room, isHardLimitUsage as not-capped — whereas 0 would be a number
// cux invented. The direction is the same one staleness takes: an answer we
// cannot support must not become a reason to move a session.
func (u AccountUsage) Settled(now time.Time) AccountUsage {
	elapsed := func(w *Window) bool {
		return w != nil && w.ResetsAt != nil && !w.ResetsAt.After(now)
	}
	out := u
	if elapsed(out.FiveHour) {
		out.FiveHour = nil
	}
	if elapsed(out.SevenDay) {
		out.SevenDay = nil
	}
	// Rebuilt rather than edited in place: out shares u's map, and a caller
	// asking what is settled *now* must not have the reading it passed in
	// quietly rewritten underneath it.
	if len(out.Models) > 0 {
		models := make(map[string]*Window, len(out.Models))
		for name, w := range out.Models {
			if !elapsed(w) {
				models[name] = w
			}
		}
		out.Models = models
	}
	return out
}

// IsOverThresholdAt is IsOverThreshold over a Settled reading, and is what
// every swap decision should call.
//
// IsOverThreshold itself consults only utilization, so a window that reset
// minutes ago still reports its pre-reset figure — enough to swap a session
// off an account that has in fact just recovered. Kept as a separate
// function rather than a signature change so the plain predicate stays
// available for rendering, where the raw recorded number is what a caller
// asking for it wants.
func IsOverThresholdAt(u AccountUsage, t Thresholds, now time.Time) (over bool, reason string) {
	return IsOverThreshold(u.Settled(now), t)
}

// --- internals -------------------------------------------------------------

// apiResponse mirrors the documented (and observed) shape of the
// usage endpoint. Extra fields not modelled here are silently dropped
// by the JSON decoder, which is the right behavior — the API has
// added several non-standard windows over time and may add more.
// parseResponse reads the usage endpoint's body into an AccountUsage. The
// wire shape and the cache shape are the same flat object, so this is the
// same decode — which is what keeps a newly-named window from needing a code
// change in two places instead of none.
func parseResponse(body []byte) (AccountUsage, error) {
	var u AccountUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return AccountUsage{}, err
	}
	return u, nil
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

// HasSwitchCapacity reports whether an account can take a session right now.
// It is the one rule behind both the /switch precheck in the hook and the
// wrapper's own target resolution: when the two disagreed, the hook let a
// switch through that the wrapper could not perform, and claude was stopped
// and relaunched on the very same seat.
//
// A reading nobody can vouch for reads as room. Refusing on one is the #37
// failure mode, and a target that turns out to be unusable is struck off and
// re-chosen by completeSwap rather than ending the session.
func HasSwitchCapacity(u AccountUsage, t Thresholds, now time.Time) bool {
	if u.StaleReading(now) {
		return true
	}
	u = u.Settled(now)
	if u.TokenExpired {
		return false
	}
	if u.SevenDay != nil && u.SevenDay.Utilization >= 100 {
		return false
	}
	cap5 := t.FiveHour
	if cap5 == 0 || cap5 == 100 {
		cap5 = 90
	}
	return u.FiveHour == nil || u.FiveHour.Utilization < float64(cap5)
}
