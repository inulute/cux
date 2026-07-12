// Package strategy decides which managed account to swap to when the
// wrapper has determined a swap is needed.
//
// Three modes:
//
//   - Drain: use one account until its 7-day cap is near, then move
//     to the next. Either an explicit priority order is configured,
//     or the strategy auto-orders by highest 7-day utilisation
//     (drain the closest-to-limit account first).
//   - Balanced: spread load by always picking the account with the
//     lowest 7-day utilisation, breaking ties by lowest 5-hour
//     utilisation.
//   - Manual: never picks; PickNext returns ok=false. The user must
//     supply an explicit target via /switch.
//
// Drain mode also implements "5-hour recovery": ShouldRebalance
// returns the account to swap *back to* when a higher-priority
// account has recovered from its 5-hour limit. The wrapper calls this
// at every Stop signal so swaps stay on the priority account whenever
// it's healthy.
//
// This package is intentionally independent of `store` and
// `claudecfg` — it works on the small `Candidate` type, which the
// wrapper builds from store data. That keeps tests trivial.
package strategy

import (
	"sort"
	"strconv"
	"strings"

	"github.com/inulute/cux/internal/usage"
)

// Kind selects which strategy to apply.
type Kind int

const (
	KindDrain Kind = iota
	KindBalanced
	KindManual
)

func (k Kind) String() string {
	switch k {
	case KindDrain:
		return "drain"
	case KindBalanced:
		return "balanced"
	case KindManual:
		return "manual"
	default:
		return "drain"
	}
}

// ParseKind accepts the string form a user types in cux config and
// returns the corresponding Kind, defaulting to Drain on garbage.
func ParseKind(s string) Kind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "balanced":
		return KindBalanced
	case "manual":
		return KindManual
	case "drain", "":
		return KindDrain
	default:
		return KindDrain
	}
}

// Candidate is the minimal account shape strategy needs. The wrapper
// converts its store.Account list into Candidates before calling.
type Candidate struct {
	Email    string
	Slot     int    // stable seat identifier; 0 when unknown (e.g. the live account)
	CacheKey string // usage-cache key; falls back to Email when empty
}

// cacheKey returns the key to use for cache lookups. When CacheKey is set
// (e.g. organizationUuid for accounts sharing an email) it is used; otherwise
// Email is used for backward compatibility.
func (c Candidate) cacheKey() string {
	if c.CacheKey != "" {
		return c.CacheKey
	}
	return c.Email
}

// sameSeat reports whether two candidates are the same seat. Emails are
// not unique — the same address can hold a personal subscription and
// one seat per organization — so compare by cache key (one per seat)
// whenever both sides carry one, and only fall back to email for
// legacy candidates that predate seat keys.
func sameSeat(a, b Candidate) bool {
	if a.CacheKey != "" && b.CacheKey != "" {
		return a.CacheKey == b.CacheKey
	}
	return a.Email == b.Email
}

// Pick is the strategy's answer. Reason is a short human-readable
// label included in the swap-history log.
type Pick struct {
	Email  string
	Slot   int // seat identifier; 0 when the pick predates slot tracking
	Reason string
}

// Identifier returns the string to hand to switcher.SwitchTo. Slots are
// unambiguous (emails are not, see sameSeat), so prefer the slot number
// whenever the pick carries one.
func (p Pick) Identifier() string {
	if p.Slot > 0 {
		return strconv.Itoa(p.Slot)
	}
	return p.Email
}

// PickNext chooses an account to swap to.
//
//   - kind: which strategy
//   - order: priority list (only consulted by KindDrain; entries are emails)
//   - accounts: every managed account
//   - current: the account currently active
//   - cache: latest usage snapshot (may be empty for fresh installs)
//   - thresholds: used to check whether a candidate is over its cap
//
// Returns ok=false in Manual mode, when there are no candidates, or
// when no candidate has any spare capacity.
func PickNext(
	kind Kind,
	order []string,
	accounts []Candidate,
	current Candidate,
	cache usage.Cache,
	thresholds usage.Thresholds,
) (Pick, bool) {
	switch kind {
	case KindManual:
		return Pick{}, false
	case KindBalanced:
		return pickBalanced(accounts, current, cache, thresholds)
	default:
		return pickDrain(order, accounts, current, cache, thresholds)
	}
}

// ShouldRebalance — drain mode only — returns the priority account to
// swap *back to* when the user is on a temp account but the priority
// account is now healthy. Returns ok=false in any other mode, or when
// no rebalance is warranted.
//
// Called at every Stop signal so the wrapper can hop back to a
// preferred account as soon as a 5-hour window resets.
func ShouldRebalance(
	kind Kind,
	order []string,
	accounts []Candidate,
	current Candidate,
	cache usage.Cache,
	thresholds usage.Thresholds,
) (Pick, bool) {
	if kind != KindDrain {
		return Pick{}, false
	}

	var priority *Candidate
	if len(order) > 0 {
		// First healthy account in user-defined order.
		for _, email := range order {
			c := findByEmail(accounts, email)
			if c == nil {
				continue
			}
			if !isAvailable(cache, c.cacheKey()) {
				continue
			}
			if u, ok := cache[c.cacheKey()]; ok {
				if over, _ := usage.IsOverThreshold(u, thresholds); over {
					continue
				}
			}
			// Never rebalance ONTO a model-capped account: the hop is
			// proactive, and landing a heavy-Opus session on a capped
			// seat trades a working account for an instant rate limit —
			// and a bounce right back at the next Stop.
			if modelCapped(cache, c.cacheKey()) {
				continue
			}
			priority = c
			break
		}
	} else {
		// Auto-order: highest 7d util that's still under threshold.
		// Treats "no cache entry" as available — fresh installs work.
		var best *Candidate
		var bestUtil float64 = -1
		for i := range accounts {
			c := &accounts[i]
			if sameSeat(*c, current) {
				continue
			}
			if !isAvailable(cache, c.cacheKey()) {
				continue
			}
			if u, ok := cache[c.cacheKey()]; ok {
				if over, _ := usage.IsOverThreshold(u, thresholds); over {
					continue
				}
			}
			if modelCapped(cache, c.cacheKey()) {
				continue
			}
			u := sevenDayUtil(cache, c.cacheKey())
			if u > bestUtil {
				bestUtil = u
				best = c
			}
		}
		priority = best
	}

	if priority == nil || sameSeat(*priority, current) {
		return Pick{}, false
	}
	return Pick{Email: priority.Email, Slot: priority.Slot, Reason: "rebalance to priority account"}, true
}

// --- internals -----------------------------------------------------------

func pickDrain(
	order []string,
	accounts []Candidate,
	current Candidate,
	cache usage.Cache,
	thresholds usage.Thresholds,
) (Pick, bool) {
	ordered := orderedCandidates(order, accounts, current, cache)
	if len(ordered) == 0 {
		return Pick{}, false
	}

	// Pass 1: 7d under threshold (or below 95 default).
	cap7 := thresholds.SevenDay
	if cap7 == 0 || cap7 == 100 {
		cap7 = 95
	}
	cap5 := thresholds.FiveHour
	if cap5 == 0 || cap5 == 100 {
		cap5 = 90
	}
	// Each pass runs twice: first over candidates whose model-specific
	// weekly windows (Opus/Sonnet) still have room, then over the rest.
	// Model windows never make an account ineligible — cux cannot know
	// which model the wrapped session will ask for next — but a
	// model-capped account is a worse first choice: a heavy-Opus
	// session swapped onto it rate-limits on its next call. When every
	// candidate is model-capped the second sweep restores today's pick,
	// so a pool is never stranded by this preference.
	for _, preferModelClear := range []bool{true, false} {
		for _, c := range ordered {
			if preferModelClear && modelCapped(cache, c.cacheKey()) {
				continue
			}
			if !isAvailable(cache, c.cacheKey()) {
				continue
			}
			if fiveHourUtil(cache, c.cacheKey()) < float64(cap5) && sevenDayUtil(cache, c.cacheKey()) < float64(cap7) {
				return Pick{Email: c.Email, Slot: c.Slot, Reason: "drain: 7d under cap"}, true
			}
		}
	}

	// Pass 2: 5h has any room — but never pick a 7D-hard-full account.
	// A 7D-at-100% account has no recoverable capacity in this window
	// regardless of 5h utilisation.
	for _, preferModelClear := range []bool{true, false} {
		for _, c := range ordered {
			if preferModelClear && modelCapped(cache, c.cacheKey()) {
				continue
			}
			if !isAvailable(cache, c.cacheKey()) {
				continue
			}
			if fiveHourUtil(cache, c.cacheKey()) < float64(cap5) && sevenDayUtil(cache, c.cacheKey()) < 100 {
				return Pick{Email: c.Email, Slot: c.Slot, Reason: "drain: 5h has room"}, true
			}
		}
	}

	return Pick{}, false
}

func pickBalanced(accounts []Candidate, current Candidate, cache usage.Cache, thresholds usage.Thresholds) (Pick, bool) {
	candidates := make([]Candidate, 0, len(accounts))
	for _, c := range accounts {
		if sameSeat(c, current) {
			continue
		}
		if !isAvailable(cache, c.cacheKey()) {
			continue
		}
		if !hasFiveHourCapacity(cache, c.cacheKey(), thresholds) {
			continue
		}
		if !hasSevenDayCapacity(cache, c.cacheKey()) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return Pick{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// Model-capped accounts stay eligible but sort last — see the
		// rationale on modelCapped.
		mi, mj := modelCapped(cache, candidates[i].cacheKey()), modelCapped(cache, candidates[j].cacheKey())
		if mi != mj {
			return !mi
		}
		ai, bi := sevenDayUtil(cache, candidates[i].cacheKey()), sevenDayUtil(cache, candidates[j].cacheKey())
		if ai != bi {
			return ai < bi
		}
		return fiveHourUtil(cache, candidates[i].cacheKey()) < fiveHourUtil(cache, candidates[j].cacheKey())
	})
	return Pick{Email: candidates[0].Email, Slot: candidates[0].Slot, Reason: "balanced: lowest 7d"}, true
}

// orderedCandidates returns the drain-mode evaluation order: the
// user's `order` list filtered to currently-managed non-current
// accounts, or — if `order` is empty — accounts sorted by descending
// 7-day utilisation (drain closest-to-limit first).
func orderedCandidates(order []string, accounts []Candidate, current Candidate, cache usage.Cache) []Candidate {
	if len(order) == 0 {
		out := make([]Candidate, 0, len(accounts))
		for _, c := range accounts {
			if sameSeat(c, current) {
				continue
			}
			out = append(out, c)
		}
		sort.SliceStable(out, func(i, j int) bool {
			return sevenDayUtil(cache, out[i].cacheKey()) > sevenDayUtil(cache, out[j].cacheKey())
		})
		return out
	}
	out := make([]Candidate, 0, len(order))
	for _, email := range order {
		// An email in the priority list may match several seats (personal
		// + org). Keep them all, in list order, minus the current seat.
		for i := range accounts {
			if accounts[i].Email != email || sameSeat(accounts[i], current) {
				continue
			}
			out = append(out, accounts[i])
		}
	}
	return out
}

// modelCapped reports whether any model-specific weekly window
// (Opus/Sonnet) sits at its hard cap. Only >=100 counts: user
// thresholds express intent about the account-wide windows, not the
// per-model ones, and a model window the plan does not report stays
// nil — which reads as room, like every other missing window.
func modelCapped(cache usage.Cache, key string) bool {
	u, ok := cache[key]
	if !ok {
		return false
	}
	for _, w := range []*usage.Window{u.SevenDayOpus, u.SevenDaySonnet} {
		if w != nil && w.Utilization >= 100 {
			return true
		}
	}
	return false
}

func isAvailable(cache usage.Cache, email string) bool {
	u, ok := cache[email]
	if !ok {
		// No cache entry ⇒ assume available; lets fresh installs pick
		// any account before usage has been polled.
		return true
	}
	return !u.TokenExpired
}

func sevenDayUtil(cache usage.Cache, email string) float64 {
	if u, ok := cache[email]; ok && u.SevenDay != nil {
		return u.SevenDay.Utilization
	}
	return 0
}

func fiveHourUtil(cache usage.Cache, email string) float64 {
	if u, ok := cache[email]; ok && u.FiveHour != nil {
		return u.FiveHour.Utilization
	}
	return 0
}

func hasFiveHourCapacity(cache usage.Cache, email string, thresholds usage.Thresholds) bool {
	u, ok := cache[email]
	if !ok || u.FiveHour == nil {
		return true
	}
	cap5 := thresholds.FiveHour
	if cap5 == 0 || cap5 == 100 {
		cap5 = 90
	}
	return u.FiveHour.Utilization < float64(cap5)
}

// hasSevenDayCapacity returns false only when 7D utilisation is at the hard
// 100% ceiling. Unlike hasFiveHourCapacity this uses the hard limit, not a
// user-configured threshold, because a 7D-at-100% account has zero usable
// capacity regardless of strategy thresholds.
func hasSevenDayCapacity(cache usage.Cache, email string) bool {
	u, ok := cache[email]
	if !ok || u.SevenDay == nil {
		return true
	}
	return u.SevenDay.Utilization < 100
}

func findByEmail(accounts []Candidate, email string) *Candidate {
	for i := range accounts {
		if accounts[i].Email == email {
			return &accounts[i]
		}
	}
	return nil
}
