// Package creds reads and writes OAuth tokens for Claude Code.
//
// Two distinct storage roles:
//
//   - "Live" credentials are wherever Claude Code itself reads from.
//     cux must write here to actually change the active account.
//     macOS:     Keychain generic password; see macKeychainServices.
//     Linux/Win: File at ~/.claude/.credentials.json, mode 0600.
//
//   - "Backup" credentials are cux's per-account stash. On macOS/Windows
//     they live in the OS keystore under our own service name "cux-backup"
//     so they're encrypted at rest by the OS. On Linux there is no
//     guaranteed keystore daemon, so we fall back to 0600-mode files
//     under our backup directory (the same trade-off cc-account-switcher
//     and claude-swap make).
//
// Tokens are opaque strings to most callers. The one exception is
// `ExtractAccessToken`, a tiny convenience that pulls the OAuth bearer
// out of a blob so the wrapper can call the usage API without
// re-implementing the parse in two places.
//
// RefreshBlob and IsTokenExpired provide a first-class token-refresh
// path so cux never needs Claude Code to be running in order to obtain
// a fresh access token.
package creds

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// macKeychainServices are the generic-password service names Claude Code is
// known to keep its credentials under on macOS, in the order we try them.
//
// Most installs have only the first, and it holds everything. Some installs
// — the name suggests managed/enterprise provisioning — also have the
// second, and then the account login lives there while the classic item is
// left holding just the `mcpOAuth` block for MCP servers (issue #42).
// Because the classic item still *exists* in that case, selecting by name
// silently yields credentials with no account token, so the live item is
// chosen by content instead: see selectLiveItem.
var macKeychainServices = []string{
	"Claude Code-credentials",
	"Orca Claude Code Managed Credentials",
}

// keychainExitNotFound is `security`'s exit code for "no such item".
// Any other non-zero exit is a real failure (a denied keychain prompt, a
// locked keychain) and must not be reported as "not logged in".
const keychainExitNotFound = 44

// backupKeyringService is cux's own namespace inside the OS keystore on
// macOS/Windows. Distinct from claude-swap's "claude-code" so a user who
// has both tools installed sees no overlap.
const backupKeyringService = "cux-backup"

// OAuth token-refresh constants extracted from the Claude Code binary.
// The endpoint and client_id are fixed for Claude Code's public OAuth app.
const (
	claudeTokenEndpoint = "https://platform.claude.com/v1/oauth/token"
	claudeClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// refreshBuffer is how far before expiry we proactively refresh.
	// 5 minutes gives a safety margin without being over-eager.
	refreshBuffer = 5 * time.Minute
)

// ErrNotFound is returned by ReadLive when no live credentials exist
// (user never logged in, or just logged out).
var ErrNotFound = errors.New("creds: live credentials not found")

// ErrNoAccountToken is returned when credentials *were* found but carry no
// claudeAiOauth.accessToken — an MCP-only keychain item, or a backup blob
// captured from one.
//
// Kept distinct from ErrNotFound on purpose. The two conditions send a user
// looking in completely different places: ErrNotFound means "log in",
// ErrNoAccountToken means "the login is somewhere cux did not look, or the
// stored copy is not a login at all". Issue #42 was hard to diagnose
// precisely because both reported "live credentials not found".
var ErrNoAccountToken = errors.New("creds: credentials found but they carry no account token (claudeAiOauth)")

// envCredsBackend forces the plain-file storage backend on any
// platform when set to "file". Its primary purpose is test isolation:
// on macOS/Windows both the live and backup stores live in the OS
// keystore, which HOME/XDG_DATA_HOME redirection cannot reach, so
// without this a `go test` run reads — and can write — the real
// keychain (issue #7). File paths, by contrast, all resolve through
// HOME/XDG and land inside the test's temp directory.
const envCredsBackend = "CUX_CREDS_BACKEND"

func fileBackendForced() bool {
	return os.Getenv(envCredsBackend) == "file"
}

// ReadLive returns the active credential blob Claude Code is currently
// using. The format is otherwise opaque to us — we only check that it
// carries an account token.
//
// Credentials that exist but hold no claudeAiOauth.accessToken come back as
// ErrNoAccountToken rather than as a blob, so no caller can stash one as a
// backup or write it back live. The macOS path already selects by content;
// the check lives here so the file backends answer identically and the
// sentinel is not a platform quirk.
func ReadLive() (string, error) {
	var (
		blob string
		err  error
	)
	if runtime.GOOS == "darwin" && !fileBackendForced() {
		blob, err = readLiveMacOS()
	} else {
		blob, err = readLiveFile()
	}
	if err != nil {
		return "", err
	}
	if err := requireAccountToken(blob); err != nil {
		return "", err
	}
	return blob, nil
}

// WriteLive replaces the live credential blob Claude Code reads.
// On macOS it goes to the keychain; on Linux/Windows it goes to the
// file at ~/.claude/.credentials.json with mode 0600.
func WriteLive(blob string) error {
	if blob == "" {
		return errors.New("creds: refusing to write empty live credentials")
	}
	if err := requireAccountToken(blob); err != nil {
		return fmt.Errorf("creds: refusing to write live credentials with no account token — that would sign you out: %w", err)
	}
	if runtime.GOOS == "darwin" && !fileBackendForced() {
		return writeLiveMacOS(blob)
	}
	return writeLiveFile(blob)
}

// ReadBackup returns the saved credential blob for one account, or
// ErrNotFound if there is no backup for it.
func ReadBackup(slot int, email string) (string, error) {
	switch {
	case runtime.GOOS == "linux" || fileBackendForced():
		return readBackupFile(slot, email)
	case runtime.GOOS == "darwin":
		return readBackupKeychainMacOS(slot, email)
	}
	return readBackupKeyring(slot, email)
}

// SlotRef names one managed slot for the backup-scanning helpers below,
// which cannot take a store.State without creds ceasing to be a leaf.
type SlotRef struct {
	Slot  int
	Email string
}

// TokenFingerprint hashes the account token inside a credentials blob, so two
// slots can be compared for token identity without a raw bearer being held
// for comparison or reaching an error message. ok is false for a blob that
// cannot be parsed or carries no account token.
func TokenFingerprint(blob string) (fingerprint string, ok bool) {
	tok, err := ExtractAccessToken(blob)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:]), true
}

// SlotHoldingToken returns the first slot in slots, other than skipSlot,
// whose stored credentials carry the same account token as blob.
//
// This is the check that catches an identity and a token belonging to
// different accounts. Claude Code keeps the two in separate places and they
// can disagree — `claude auth login` has been seen writing the token to the
// default location while the identity file still named the previous account
// (issue #46) — and pairing them files account B's token under account A's
// name. Both slots then poll usage with the same token and each reports the
// other's limits, which nothing downstream can detect because every figure
// looks plausible.
//
// A shared token is the reliable tell. Two real logins never carry the same
// access token, and that holds for twin seats — one email across a personal
// and an organization account — which authenticate separately. So a token
// already present under another slot is always this bug.
//
// Best-effort by design: a slot whose backup cannot be read, or which holds
// no account token, is skipped rather than treated as an error. Unreadable
// credentials are their own live failure mode and must not turn into a
// refusal to do the caller's work.
func SlotHoldingToken(slots []SlotRef, skipSlot int, blob string) (SlotRef, bool) {
	want, ok := TokenFingerprint(blob)
	if !ok {
		return SlotRef{}, false
	}
	for _, ref := range slots {
		if ref.Slot == skipSlot {
			continue
		}
		stored, err := ReadBackup(ref.Slot, ref.Email)
		if err != nil {
			continue
		}
		if got, ok := TokenFingerprint(stored); ok && got == want {
			return ref, true
		}
	}
	return SlotRef{}, false
}

// SharedTokenSlots returns the groups of slots that hold the same account
// token as each other. Every group is a pair of accounts reporting one
// account's usage under two names.
//
// Diagnostic counterpart to SlotHoldingToken: that one prevents the pairing
// at the write boundary, this one finds pools already corrupted by a build
// that had no such guard.
func SharedTokenSlots(slots []SlotRef) [][]SlotRef {
	byFingerprint := map[string][]SlotRef{}
	var order []string
	for _, ref := range slots {
		stored, err := ReadBackup(ref.Slot, ref.Email)
		if err != nil {
			continue
		}
		fp, ok := TokenFingerprint(stored)
		if !ok {
			continue
		}
		if _, seen := byFingerprint[fp]; !seen {
			order = append(order, fp)
		}
		byFingerprint[fp] = append(byFingerprint[fp], ref)
	}
	var out [][]SlotRef
	for _, fp := range order {
		if group := byFingerprint[fp]; len(group) > 1 {
			out = append(out, group)
		}
	}
	return out
}

// BackupState is the health of one slot's stored login, as reported by
// CheckBackup.
type BackupState int

const (
	// BackupOK: a login is stored and carries an account token.
	BackupOK BackupState = iota
	// BackupMissing: nothing is stored for this slot at all. On
	// macOS/Windows the keystore item is gone; on Linux the file is.
	BackupMissing
	// BackupNoToken: something is stored but carries no account token, so
	// switching to the slot would sign the user out (issue #42).
	BackupNoToken
	// BackupUnreadable: the store itself refused the read — a locked
	// keychain, a denied prompt. Kept apart from BackupMissing because the
	// login may well still be there; the two need different advice.
	BackupUnreadable
)

// Describe renders the state as a short phrase for a status line.
func (b BackupState) Describe() string {
	switch b {
	case BackupOK:
		return "stored login OK"
	case BackupMissing:
		return "no stored login"
	case BackupNoToken:
		return "stored login carries no account token"
	case BackupUnreadable:
		return "stored login could not be read"
	}
	return "unknown"
}

// CheckBackup reports whether slot's stored login could actually be used,
// by reading it rather than inferring from a failed poll.
//
// This is the question `cux status` could not answer. When the credential
// store empties out, every symptom shows up somewhere else — usage stops
// refreshing, swaps stop firing — and none of them names the cause, so a
// pool can look merely quiet for as long as it takes someone to run a
// direct read (issue #46). Any error is returned alongside the state for
// callers that want the detail; the state alone is enough to act on.
func CheckBackup(slot int, email string) (BackupState, error) {
	blob, err := ReadBackup(slot, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return BackupMissing, err
	case err != nil:
		return BackupUnreadable, err
	case blob == "":
		return BackupMissing, ErrNotFound
	}
	if _, err := ExtractAccessToken(blob); err != nil {
		return BackupNoToken, err
	}
	return BackupOK, nil
}

// WriteBackup saves the credential blob for one account.
func WriteBackup(slot int, email, blob string) error {
	if blob == "" {
		return errors.New("creds: refusing to write empty backup credentials")
	}
	if err := requireAccountToken(blob); err != nil {
		return fmt.Errorf("creds: refusing to back up credentials with no account token — the slot would sign you out when switched to: %w", err)
	}
	switch {
	case runtime.GOOS == "linux" || fileBackendForced():
		return writeBackupFile(slot, email, blob)
	case runtime.GOOS == "darwin":
		return writeBackupKeychainMacOS(slot, email, blob)
	}
	return writeBackupKeyring(slot, email, blob)
}

// ExtractAccessToken pulls the OAuth bearer token out of a credentials
// blob (the same JSON shape Claude Code writes to .credentials.json).
// Returns ErrNotFound for an empty blob and ErrNoAccountToken for a blob
// that parses but has no claudeAiOauth.accessToken.
//
// The token is never logged; this helper does not surface it in any
// error message that propagates out of the package.
func ExtractAccessToken(blob string) (string, error) {
	if blob == "" {
		return "", ErrNotFound
	}
	var doc struct {
		ClaudeAIOAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		return "", fmt.Errorf("creds: parse blob: %w", err)
	}
	if doc.ClaudeAIOAuth.AccessToken == "" {
		return "", ErrNoAccountToken
	}
	return doc.ClaudeAIOAuth.AccessToken, nil
}

// hasAccountToken reports whether blob carries a usable account token.
func hasAccountToken(blob string) bool {
	_, err := ExtractAccessToken(blob)
	return err == nil
}

// requireAccountToken rejects a blob that is not a usable login. Writing
// one over the live credentials signs the user out with no visible error,
// and stashing one as a backup builds a slot that signs them out later when
// it is switched to — the second-order damage in issue #42, where an MCP-only
// keychain item was captured by `cux add` as if it were an account login.
func requireAccountToken(blob string) error {
	_, err := ExtractAccessToken(blob)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return ErrNoAccountToken
	}
	return err
}

// IsTokenExpired reports whether the access token in blob has already
// expired or will expire within the refresh buffer window. Returns false
// when the expiry field is absent or unparseable (fail-open so callers
// still attempt the API call and handle a real 401 themselves).
func IsTokenExpired(blob string) bool {
	var doc struct {
		ClaudeAIOAuth struct {
			ExpiresAt int64 `json:"expiresAt"` // milliseconds since Unix epoch
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(blob), &doc); err != nil || doc.ClaudeAIOAuth.ExpiresAt == 0 {
		return false
	}
	return time.Until(time.UnixMilli(doc.ClaudeAIOAuth.ExpiresAt)) < refreshBuffer
}

// RefreshBlob exchanges the refresh token inside blob for a new access
// token, updates the blob in-place, and returns the updated copy.
// The original blob is returned alongside any error so callers can fall
// back gracefully.
func RefreshBlob(blob string) (string, error) {
	// Parse the entire blob as a raw map so unknown top-level keys survive.
	var rawDoc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &rawDoc); err != nil {
		return blob, fmt.Errorf("creds: parse blob: %w", err)
	}

	// Parse the claudeAiOauth sub-object as a raw map to preserve all fields
	// (subscriptionType, rateLimitTier, scopes, etc.).
	rawOAuth, ok := rawDoc["claudeAiOauth"]
	if !ok {
		return blob, fmt.Errorf("creds: no claudeAiOauth block in blob")
	}
	var oauthMap map[string]json.RawMessage
	if err := json.Unmarshal(rawOAuth, &oauthMap); err != nil {
		return blob, fmt.Errorf("creds: parse claudeAiOauth: %w", err)
	}

	// Extract the refresh token.
	var refreshToken string
	rt, hasRT := oauthMap["refreshToken"]
	if !hasRT {
		return blob, fmt.Errorf("creds: no refreshToken in blob")
	}
	if err := json.Unmarshal(rt, &refreshToken); err != nil || refreshToken == "" {
		return blob, fmt.Errorf("creds: empty or unparseable refreshToken")
	}

	// Call the token endpoint.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {claudeClientID},
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(claudeTokenEndpoint, form)
	if err != nil {
		return blob, fmt.Errorf("creds: token refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return blob, fmt.Errorf("creds: read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return blob, fmt.Errorf("creds: token refresh HTTP %d: %s", resp.StatusCode, snip)
	}

	// Parse the response.
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return blob, fmt.Errorf("creds: parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return blob, fmt.Errorf("creds: token response missing access_token")
	}

	// Patch the oauth map with the new values.
	newAT, _ := json.Marshal(tokenResp.AccessToken)
	oauthMap["accessToken"] = newAT

	if tokenResp.RefreshToken != "" {
		newRT, _ := json.Marshal(tokenResp.RefreshToken)
		oauthMap["refreshToken"] = newRT
	}
	if tokenResp.ExpiresIn > 0 {
		expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000
		newExp, _ := json.Marshal(expiresAt)
		oauthMap["expiresAt"] = newExp
	}

	// Rebuild the blob.
	newOAuth, err := json.Marshal(oauthMap)
	if err != nil {
		return blob, fmt.Errorf("creds: marshal updated oauth block: %w", err)
	}
	rawDoc["claudeAiOauth"] = newOAuth
	newBlob, err := json.Marshal(rawDoc)
	if err != nil {
		return blob, fmt.Errorf("creds: marshal updated blob: %w", err)
	}
	return string(newBlob), nil
}

// DeleteBackup removes the saved credential blob for one account.
// Missing entries are not an error — deletion is idempotent.
func DeleteBackup(slot int, email string) error {
	switch {
	case runtime.GOOS == "linux" || fileBackendForced():
		return deleteBackupFile(slot, email)
	case runtime.GOOS == "darwin":
		return deleteBackupKeychainMacOS(slot, email)
	}
	return deleteBackupKeyring(slot, email)
}

// --- macOS live (security CLI) --------------------------------------------

// We shell out to `security` rather than going through go-keyring so we
// inherit Claude Code's exact keychain semantics (single-line generic
// password, no extra metadata) and don't risk the Go library prompting
// the user for keychain access on every read.

// macKeychainItem is one existing generic-password item we found.
type macKeychainItem struct {
	service string
	account string
	blob    string
}

// errKeychainItemAbsent marks `security` exit 44 (no such item) so the
// enumeration can skip a missing service without hiding a real failure.
var errKeychainItemAbsent = errors.New("creds: keychain item absent")

// readMacKeychainSecret returns the secret stored under one service name.
// When account is supplied, it selects that exact item instead of whichever
// item `security` happens to return first for the service.
func readMacKeychainSecret(service string, account ...string) (string, error) {
	args := []string{"find-generic-password", "-s", service}
	if len(account) > 0 {
		args = append(args, "-a", account[0])
	}
	args = append(args, "-w")
	out, err := exec.Command("security", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == keychainExitNotFound {
				return "", errKeychainItemAbsent
			}
			return "", fmt.Errorf("creds: security find %q: exit %d: %s",
				service, ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("creds: security find %q: %w", service, err)
	}
	return trimTrailingNewline(string(out)), nil
}

// findMacKeychainItems returns every known credentials item that exists, in
// macKeychainServices order. `security find-generic-password -s` returns an
// arbitrary single match, so the metadata-only dump supplies every account
// and each secret is then read by its exact (service, account) pair.
//
// This deliberately enumerates on every read. A fast name-only result that
// has a token can still be stale while another account under the same service
// is Claude Code's active item, so its contents cannot safely skip the dump.
// The name-only lookup still runs afterward so an unquoted account or an
// empty successful dump cannot make an item reachable by the old path vanish.
func findMacKeychainItems() ([]macKeychainItem, error) {
	dump, err := exec.Command("security", "dump-keychain").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("creds: security dump-keychain: exit %d: %s",
				ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("creds: security dump-keychain: %w", err)
	}

	var (
		out      []macKeychainItem
		firstErr error
	)
	for _, service := range macKeychainServices {
		accounts := parseMacKeychainAccounts(string(dump), service)
		for _, account := range accounts {
			blob, err := readMacKeychainSecret(service, account)
			if err != nil {
				if !errors.Is(err, errKeychainItemAbsent) && firstErr == nil {
					firstErr = err
				}
				continue
			}
			out = append(out, macKeychainItem{service: service, account: account, blob: blob})
		}

		blob, err := readMacKeychainSecret(service)
		if err != nil {
			if !errors.Is(err, errKeychainItemAbsent) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		duplicate := false
		for _, item := range out {
			if item.service == service && item.blob == blob {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, macKeychainItem{service: service, blob: blob})
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, ErrNotFound
	}
	return out, nil
}

// selectLiveItem picks the item cux should treat as the live credentials.
// Service order wins first; among items for one service, a non-expired token
// wins, or the first expired token is kept so the refresh path can recover it.
//
// When no item has one it returns the first item that exists along with
// ErrNoAccountToken. The item is still useful to the write path — restoring
// a backup should land in the item that is already there rather than
// creating a new one — while the read path discards it and surfaces the
// error, so no caller can mistake an MCP-only blob for a login.
func selectLiveItem(items []macKeychainItem) (macKeychainItem, error) {
	if len(items) == 0 {
		return macKeychainItem{}, ErrNotFound
	}
	for _, service := range macKeychainServices {
		firstToken := -1
		for i := range items {
			if items[i].service != service || !hasAccountToken(items[i].blob) {
				continue
			}
			if firstToken == -1 {
				firstToken = i
			}
			if !IsTokenExpired(items[i].blob) {
				return items[i], nil
			}
		}
		if firstToken != -1 {
			return items[firstToken], nil
		}
	}
	return items[0], ErrNoAccountToken
}

func readLiveMacOS() (string, error) {
	items, err := findMacKeychainItems()
	if err != nil {
		return "", err
	}
	it, err := selectLiveItem(items)
	if err != nil {
		return "", err
	}
	return it.blob, nil
}

// keychainAcctRe and keychainServiceRe match attributes in `security`'s item
// dump. Each record starts with a keychain line, which lets the parser keep an
// account paired with its own service rather than matching across records.
// Only the quoted form is accepted: the attribute can also print as <NULL>
// or as a hex literal, and in both cases we would rather fall back to $USER
// than write under a mangled account name.
var (
	keychainAcctRe    = regexp.MustCompile(`(?m)^\s*"acct"<blob>="(.*)"\s*$`)
	keychainServiceRe = regexp.MustCompile(`(?m)^\s*"svce"<blob>="(.*)"\s*$`)
)

func parseKeychainAccount(dump string) string {
	m := keychainAcctRe.FindStringSubmatch(dump)
	if m == nil {
		return ""
	}
	return m[1]
}

func parseMacKeychainAccounts(dump, service string) []string {
	var accounts []string
	for _, item := range strings.Split(dump, "\nkeychain:") {
		account := parseKeychainAccount(item)
		match := keychainServiceRe.FindStringSubmatch(item)
		if account != "" && match != nil && match[1] == service {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func writeLiveMacOS(blob string) error {
	// Write into whichever item the read path selects, so a switch lands
	// where Claude Code is actually looking. Falling back to the classic
	// service only when nothing exists keeps a first-ever write working.
	service := macKeychainServices[0]
	account := ""
	if items, err := findMacKeychainItems(); err == nil {
		if it, _ := selectLiveItem(items); it.service != "" {
			service = it.service
			account = it.account
		}
	}
	if account == "" {
		account = os.Getenv("USER")
	}
	cmd := exec.Command("security", "add-generic-password",
		"-U", // update if already present
		"-s", service,
		"-a", account,
		"-w", blob,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creds: security add %q: %w (%s)", service, err, out)
	}
	return nil
}

// --- Linux/Windows live (file) --------------------------------------------

func readLiveFile() (string, error) {
	b, err := os.ReadFile(paths.ClaudeCredentials())
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("creds: read %s: %w", paths.ClaudeCredentials(), err)
	}
	return string(b), nil
}

func writeLiveFile(blob string) error {
	if err := os.MkdirAll(paths.ClaudeDir(), 0o700); err != nil {
		return fmt.Errorf("creds: mkdir %s: %w", paths.ClaudeDir(), err)
	}
	return atomicfile.Write(paths.ClaudeCredentials(), []byte(blob), 0o600)
}

// --- Backup: keyring (Windows) --------------------------------------------

func backupKeyringUser(slot int, email string) string {
	// Mirror cc-account-switcher / claude-swap convention so the data
	// shape is recognisable to a user who switches tools, but under our
	// own service name to avoid actual collisions.
	return fmt.Sprintf("account-%d-%s", slot, email)
}

func readBackupKeyring(slot int, email string) (string, error) {
	v, err := keyring.Get(backupKeyringService, backupKeyringUser(slot, email))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("creds: keyring get: %w", err)
	}
	return v, nil
}

func writeBackupKeyring(slot int, email, blob string) error {
	if err := keyring.Set(backupKeyringService, backupKeyringUser(slot, email), blob); err != nil {
		return fmt.Errorf("creds: keyring set: %w", err)
	}
	return nil
}

func deleteBackupKeyring(slot int, email string) error {
	err := keyring.Delete(backupKeyringService, backupKeyringUser(slot, email))
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("creds: keyring delete: %w", err)
	}
	return nil
}

// --- Backup: Keychain via security CLI (macOS) -----------------------------

// go-keyring rejects secrets over ~3 KB on macOS: it pipes the whole
// add-generic-password command through `security -i` and errors once the
// command line exceeds 4096 bytes ("data passed to Set was too big").
// Claude Code stores every MCP server's OAuth state next to the account
// login in the same credential blob, so real-world blobs routinely blow
// past that cap and `cux add` fails. Shelling out to `security` with the
// secret as an argument — exactly what the live path above already does —
// has no such limit.
//
// The stored value keeps go-keyring's well-known-prefix base64 encoding,
// so backups written by earlier cux versions read back fine and vice
// versa.

// Prefixes go-keyring puts in front of encoded secrets.
const (
	keyringBase64Prefix = "go-keyring-base64:"
	keyringHexPrefix    = "go-keyring-encoded:"
)

func readBackupKeychainMacOS(slot int, email string) (string, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-s", backupKeyringService,
		"-wa", backupKeyringUser(slot, email))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == keychainExitNotFound {
				return "", ErrNotFound
			}
			return "", fmt.Errorf("creds: security find backup: exit %d: %s",
				ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("creds: security find backup: %w", err)
	}
	return decodeBackupValue(trimTrailingNewline(string(out)))
}

func writeBackupKeychainMacOS(slot int, email, blob string) error {
	// base64 keeps the value single-line ASCII so `security` round-trips
	// it verbatim instead of hex-mangling multiline/non-ASCII input.
	encoded := keyringBase64Prefix + base64.StdEncoding.EncodeToString([]byte(blob))
	cmd := exec.Command("security", "add-generic-password",
		"-U", // update if already present
		"-s", backupKeyringService,
		"-a", backupKeyringUser(slot, email),
		"-w", encoded,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creds: security add backup: %w (%s)", err, out)
	}
	return nil
}

func deleteBackupKeychainMacOS(slot int, email string) error {
	cmd := exec.Command("security", "delete-generic-password",
		"-s", backupKeyringService,
		"-a", backupKeyringUser(slot, email))
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "could not be found") {
			return nil // deletion is idempotent
		}
		return fmt.Errorf("creds: security delete backup: %w (%s)", err, out)
	}
	return nil
}

// decodeBackupValue undoes go-keyring's optional secret encoding so
// backups written through go-keyring (older cux versions, or the Windows
// path) read back as the original blob.
func decodeBackupValue(v string) (string, error) {
	switch {
	case strings.HasPrefix(v, keyringBase64Prefix):
		dec, err := base64.StdEncoding.DecodeString(v[len(keyringBase64Prefix):])
		if err != nil {
			return "", fmt.Errorf("creds: decode backup: %w", err)
		}
		return string(dec), nil
	case strings.HasPrefix(v, keyringHexPrefix):
		dec, err := hex.DecodeString(v[len(keyringHexPrefix):])
		if err != nil {
			return "", fmt.Errorf("creds: decode backup: %w", err)
		}
		return string(dec), nil
	}
	return v, nil
}

// --- Backup: file (Linux) -------------------------------------------------

func backupFilePath(slot int, email string) string {
	return filepath.Join(paths.AccountDir(slot, email), "credentials.json")
}

func readBackupFile(slot int, email string) (string, error) {
	b, err := os.ReadFile(backupFilePath(slot, email))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("creds: read backup: %w", err)
	}
	return string(b), nil
}

func writeBackupFile(slot int, email, blob string) error {
	dir := paths.AccountDir(slot, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creds: mkdir %s: %w", dir, err)
	}
	return atomicfile.Write(backupFilePath(slot, email), []byte(blob), 0o600)
}

func deleteBackupFile(slot int, email string) error {
	err := os.Remove(backupFilePath(slot, email))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("creds: remove backup: %w", err)
	}
	return nil
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
