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
	blob    string
}

// errKeychainItemAbsent marks `security` exit 44 (no such item) so the
// enumeration can skip a missing service without hiding a real failure.
var errKeychainItemAbsent = errors.New("creds: keychain item absent")

// readMacKeychainSecret returns the secret stored under one service name.
func readMacKeychainSecret(service string) (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", service, "-w").Output()
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
// macKeychainServices order. A missing item is not an error; a keychain that
// refuses to be read is, but only when it leaves us with nothing at all.
func findMacKeychainItems() ([]macKeychainItem, error) {
	var (
		out      []macKeychainItem
		firstErr error
	)
	for _, service := range macKeychainServices {
		blob, err := readMacKeychainSecret(service)
		if err != nil {
			if !errors.Is(err, errKeychainItemAbsent) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, macKeychainItem{service: service, blob: blob})
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, ErrNotFound
	}
	return out, nil
}

// selectLiveItem picks the item cux should treat as the live credentials:
// the first one whose blob actually carries an account token.
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
	for _, it := range items {
		if hasAccountToken(it.blob) {
			return it, nil
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

// keychainAcctRe matches the `acct` attribute in `security`'s item dump.
// Only the quoted form is accepted: the attribute can also print as <NULL>
// or as a hex literal, and in both cases we would rather fall back to $USER
// than write under a mangled account name.
var keychainAcctRe = regexp.MustCompile(`(?m)^\s*"acct"<blob>="(.*)"\s*$`)

// macKeychainAccount reads an item's own account name back from the
// keychain, returning "" when it cannot be determined.
//
// This matters because `security add-generic-password -U` matches on the
// (service, account) pair: writing under the wrong account name creates a
// *second* item beside the real one, exits 0, and leaves Claude Code still
// reading the original — a switch that reports success and changes nothing
// (issue #42). Note that `find-generic-password` reports only the first
// match for a service, so a machine that already accumulated duplicates
// from that bug needs them removed by hand.
func macKeychainAccount(service string) string {
	out, err := exec.Command("security", "find-generic-password",
		"-s", service).Output()
	if err != nil {
		return ""
	}
	return parseKeychainAccount(string(out))
}

func parseKeychainAccount(dump string) string {
	m := keychainAcctRe.FindStringSubmatch(dump)
	if m == nil {
		return ""
	}
	return m[1]
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
			account = macKeychainAccount(service)
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
