// Package store owns cux's state file (state.json) and the per-account
// oauthAccount-block backups. Credential blobs live in the creds package;
// nothing token-shaped touches store.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

const stateVersion = 1

// Account is one managed Claude Code account.
type Account struct {
	Slot     int       `json:"slot"`
	Email    string    `json:"email"`
	UUID     string    `json:"uuid"`
	OrgUUID  string    `json:"orgUuid,omitempty"`
	Alias    string    `json:"alias,omitempty"`
	AddedAt  time.Time `json:"addedAt"`
	LastUsed time.Time `json:"lastUsed,omitempty"`
}

// CacheKey returns the usage-cache key for this account. Usage limits are
// tracked per seat — one (account, organization) pair — so the key combines
// both UUIDs: two accounts sharing an email but in different orgs get
// distinct entries, and so do different accounts inside the same org
// (keying on OrgUUID alone made those collapse into one entry that every
// refresh overwrote). Falls back to OrgUUID, then email, for legacy slots
// recorded before these fields existed.
func (a Account) CacheKey() string {
	if a.UUID != "" && a.OrgUUID != "" {
		return a.UUID + "|" + a.OrgUUID
	}
	if a.OrgUUID != "" {
		return a.OrgUUID
	}
	return a.Email
}

// State is the on-disk shape of state.json.
type State struct {
	Version           int                `json:"version"`
	ActiveSlot        int                `json:"activeSlot"`
	LastUpdated       time.Time          `json:"lastUpdated"`
	Sequence          []int              `json:"sequence"` // user-visible ordering for `cux switch` rotation
	Accounts          map[int]Account    `json:"accounts"`
	Projects          map[string]Project `json:"projects,omitempty"` // per-directory account pools
	ManualSwitchEmail string             `json:"manualSwitchEmail,omitempty"`
	ManualSwitchAt    time.Time          `json:"manualSwitchAt,omitempty"`
}

var (
	ErrAccountExists  = errors.New("store: account already managed")
	ErrAccountMissing = errors.New("store: account not found")
	ErrEmptyState     = errors.New("store: no managed accounts")
)

var emailRE = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// aliasRE accepts 1–20 lowercase alphanumeric + hyphen characters that start
// with a letter. This ensures aliases can never be confused with slot numbers
// (which are purely numeric) or email addresses (which contain @).
var aliasRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// ValidateAlias returns an error if s is not a valid alias.
func ValidateAlias(s string) error {
	if !aliasRE.MatchString(s) {
		return fmt.Errorf("store: alias %q must start with a letter, contain only lowercase letters, digits, or hyphens, and be at most 20 chars", s)
	}
	return nil
}

// ErrAliasExists is returned when the requested alias is already taken.
var ErrAliasExists = errors.New("store: alias already in use")

func ValidateEmail(s string) error {
	if !emailRE.MatchString(s) {
		return fmt.Errorf("store: invalid email %q", s)
	}
	return nil
}

// Load reads state.json. If the file does not exist, returns a fresh
// empty State — callers can save it back to materialise a new install.
func Load() (*State, error) {
	path := paths.StateFile()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				Version:  stateVersion,
				Accounts: map[int]Account{},
				Sequence: []int{},
			}, nil
		}
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}

	// We accept either a typed map[int]Account or — for compatibility
	// with hand-written or migrated state — keys-as-strings. Decode via
	// an intermediate where account keys are strings, then convert.
	var raw struct {
		Version           int                `json:"version"`
		ActiveSlot        int                `json:"activeSlot"`
		LastUpdated       time.Time          `json:"lastUpdated"`
		Sequence          []int              `json:"sequence"`
		Accounts          map[string]Account `json:"accounts"`
		Projects          map[string]Project `json:"projects,omitempty"`
		ManualSwitchEmail string             `json:"manualSwitchEmail,omitempty"`
		ManualSwitchAt    time.Time          `json:"manualSwitchAt,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("store: parse %s: %w", path, err)
	}

	s := &State{
		Version:           raw.Version,
		ActiveSlot:        raw.ActiveSlot,
		LastUpdated:       raw.LastUpdated,
		Sequence:          raw.Sequence,
		Accounts:          make(map[int]Account, len(raw.Accounts)),
		Projects:          raw.Projects,
		ManualSwitchEmail: raw.ManualSwitchEmail,
		ManualSwitchAt:    raw.ManualSwitchAt,
	}
	if s.Sequence == nil {
		s.Sequence = []int{}
	}
	for k, v := range raw.Accounts {
		n, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("store: bad account key %q in state.json", k)
		}
		s.Accounts[n] = v
	}
	return s, nil
}

// Save writes state.json atomically. Callers are expected to hold the
// state lock from the lockfile package across read-modify-write cycles.
func (s *State) Save() error {
	if err := os.MkdirAll(paths.BackupRoot(), 0o700); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", paths.BackupRoot(), err)
	}

	s.Version = stateVersion
	s.LastUpdated = time.Now().UTC()

	// Re-encode accounts with string keys for stable JSON output.
	accounts := make(map[string]Account, len(s.Accounts))
	for k, v := range s.Accounts {
		accounts[strconv.Itoa(k)] = v
	}
	out := struct {
		Version           int                `json:"version"`
		ActiveSlot        int                `json:"activeSlot"`
		LastUpdated       time.Time          `json:"lastUpdated"`
		Sequence          []int              `json:"sequence"`
		Accounts          map[string]Account `json:"accounts"`
		Projects          map[string]Project `json:"projects,omitempty"`
		ManualSwitchEmail string             `json:"manualSwitchEmail,omitempty"`
		ManualSwitchAt    time.Time          `json:"manualSwitchAt,omitempty"`
	}{s.Version, s.ActiveSlot, s.LastUpdated, s.Sequence, accounts, s.Projects, s.ManualSwitchEmail, s.ManualSwitchAt}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	return atomicfile.Write(paths.StateFile(), data, 0o600)
}

// FindByEmail returns the slot for a given email, or 0 if none.
func (s *State) FindByEmail(email string) int {
	for slot, a := range s.Accounts {
		if a.Email == email {
			return slot
		}
	}
	return 0
}

// FindByIdentity returns the slot matching email+orgUUID. When orgUUID is
// non-empty it prefers an exact (email+org) match; when no exact match is
// found it falls back to an email-only match against legacy slots that have
// no OrgUUID stored. Returns 0 if no match is found.
func (s *State) FindByIdentity(email, orgUUID string) int {
	if orgUUID != "" {
		for slot, a := range s.Accounts {
			if a.Email == email && a.OrgUUID == orgUUID {
				return slot
			}
		}
	}
	// Fall back to email-only for legacy slots (OrgUUID was not stored).
	for slot, a := range s.Accounts {
		if a.Email == email && a.OrgUUID == "" {
			return slot
		}
	}
	return 0
}

// Resolve accepts either a slot number ("2") or an email and returns
// the matching account. Returns ErrAccountMissing if neither matches.
// FindByAlias returns the slot for a given alias (case-insensitive), or 0 if
// none. Aliases are stored lowercase so normalising here is defensive.
func (s *State) FindByAlias(alias string) int {
	alias = strings.ToLower(strings.TrimSpace(alias))
	for slot, a := range s.Accounts {
		if strings.ToLower(a.Alias) == alias {
			return slot
		}
	}
	return 0
}

// Resolve accepts a slot number, email, or alias and returns the matching
// account. Returns ErrAccountMissing if none matches.
func (s *State) Resolve(identifier string) (Account, error) {
	// 1. Slot number.
	if n, err := strconv.Atoi(identifier); err == nil {
		if a, ok := s.Accounts[n]; ok {
			return a, nil
		}
		return Account{}, fmt.Errorf("%w: slot %d", ErrAccountMissing, n)
	}
	// 2. Alias (before email, so "work" never hits the email validator).
	if slot := s.FindByAlias(identifier); slot != 0 {
		return s.Accounts[slot], nil
	}
	// 3. Email.
	if err := ValidateEmail(identifier); err != nil {
		// Not a number, not a known alias, not a valid email.
		return Account{}, fmt.Errorf("%w: %q (use slot number, email, or alias)", ErrAccountMissing, identifier)
	}
	if slot := s.FindByEmail(identifier); slot != 0 {
		return s.Accounts[slot], nil
	}
	return Account{}, fmt.Errorf("%w: %s", ErrAccountMissing, identifier)
}

// SetAlias assigns (or clears) the alias for a managed account. Validates the
// alias and ensures it is unique. Pass alias="" to clear.
func (s *State) SetAlias(slot int, alias string) error {
	a, ok := s.Accounts[slot]
	if !ok {
		return fmt.Errorf("%w: slot %d", ErrAccountMissing, slot)
	}
	if alias == "" {
		a.Alias = ""
		s.Accounts[slot] = a
		return nil
	}
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	if existing := s.FindByAlias(alias); existing != 0 && existing != slot {
		return fmt.Errorf("%w: %q is already used by slot %d (%s)", ErrAliasExists, alias, existing, s.Accounts[existing].Email)
	}
	a.Alias = alias
	s.Accounts[slot] = a
	return nil
}

// NextSlot returns the lowest unused positive slot number. Reusing
// holes keeps slot numbers from growing unboundedly across many
// add/remove cycles.
func (s *State) NextSlot() int {
	used := make(map[int]bool, len(s.Accounts))
	for k := range s.Accounts {
		used[k] = true
	}
	for n := 1; ; n++ {
		if !used[n] {
			return n
		}
	}
}

// Add registers a new account in state. The caller is responsible for
// having already saved its credentials and oauthAccount block via
// creds.WriteBackup and store.WriteOAuthBlockBackup.
func (s *State) Add(slot int, email, uuid, orgUUID string) error {
	if err := ValidateEmail(email); err != nil {
		return err
	}
	if _, exists := s.Accounts[slot]; exists {
		return fmt.Errorf("%w: slot %d already in use", ErrAccountExists, slot)
	}
	if existing := s.FindByIdentity(email, orgUUID); existing != 0 {
		return fmt.Errorf("%w: %s is already slot %d", ErrAccountExists, email, existing)
	}
	s.Accounts[slot] = Account{
		Slot:    slot,
		Email:   email,
		UUID:    uuid,
		OrgUUID: orgUUID,
		AddedAt: time.Now().UTC(),
	}
	s.Sequence = append(s.Sequence, slot)
	return nil
}

// Remove unregisters an account. Caller must separately delete the
// account's credential and oauth backups.
func (s *State) Remove(slot int) error {
	if _, ok := s.Accounts[slot]; !ok {
		return fmt.Errorf("%w: slot %d", ErrAccountMissing, slot)
	}
	delete(s.Accounts, slot)
	// A removed account must not linger in any project pool.
	for name := range s.Projects {
		_ = s.UnassignProjectSlot(name, slot)
	}
	out := s.Sequence[:0]
	for _, n := range s.Sequence {
		if n != slot {
			out = append(out, n)
		}
	}
	s.Sequence = out
	if s.ActiveSlot == slot {
		s.ActiveSlot = 0
	}
	return nil
}

// NextInRotation returns the slot that follows current in Sequence,
// wrapping at the end. Returns 0 if there are fewer than two accounts.
func (s *State) NextInRotation(current int) int {
	if len(s.Sequence) < 2 {
		return 0
	}
	idx := -1
	for i, n := range s.Sequence {
		if n == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		return s.Sequence[0]
	}
	return s.Sequence[(idx+1)%len(s.Sequence)]
}

// SortedSlots returns slots in Sequence order — the order users see when
// listing or rotating. New entries are appended to the end on Add.
func (s *State) SortedSlots() []int {
	if len(s.Sequence) == len(s.Accounts) {
		out := make([]int, len(s.Sequence))
		copy(out, s.Sequence)
		return out
	}
	// Sequence has drifted from Accounts (e.g. hand-edited state). Fall
	// back to numeric order so we still return every account.
	out := make([]int, 0, len(s.Accounts))
	for k := range s.Accounts {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// --- per-account oauthAccount block backup --------------------------------

// WriteOAuthBlockBackup saves the oauthAccount JSON for an account.
func WriteOAuthBlockBackup(slot int, email string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("store: refusing to back up empty oauthAccount block")
	}
	dir := paths.AccountDir(slot, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	return atomicfile.Write(filepath.Join(dir, "oauth.json"), raw, 0o600)
}

// ReadOAuthBlockBackup loads the oauthAccount JSON for an account.
func ReadOAuthBlockBackup(slot int, email string) (json.RawMessage, error) {
	b, err := os.ReadFile(filepath.Join(paths.AccountDir(slot, email), "oauth.json"))
	if err != nil {
		return nil, fmt.Errorf("store: read oauth backup: %w", err)
	}
	// Validate it's still parseable JSON before handing it back; we'd
	// rather fail here than write garbage to ~/.claude/.claude.json.
	var probe interface{}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("store: oauth backup not valid JSON: %w", err)
	}
	return json.RawMessage(b), nil
}

// DeleteOAuthBlockBackup removes the per-account directory.
func DeleteOAuthBlockBackup(slot int, email string) error {
	dir := paths.AccountDir(slot, email)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("store: remove %s: %w", dir, err)
	}
	return nil
}
