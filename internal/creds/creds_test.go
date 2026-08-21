package creds

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/paths"
)

// The blobs a real install produces: the managed item owns the account
// login, the classic item is left with only the MCP block (issue #42).
const (
	accountBlob = `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r"}}`
	mcpOnlyBlob = `{"mcpOAuth":{"some-server":{"accessToken":"not-an-account-token"}}}`
)

func TestSelectLiveItemPicksTheItemHoldingTheAccountToken(t *testing.T) {
	cases := []struct {
		name        string
		items       []macKeychainItem
		wantService string
		wantErr     error
	}{
		{
			name:        "single classic item, the common install",
			items:       []macKeychainItem{{service: "Claude Code-credentials", blob: accountBlob}},
			wantService: "Claude Code-credentials",
		},
		{
			// The #42 case: the classic item exists and looks fine to a
			// name-based lookup, but the token is in the managed item.
			name: "classic item is MCP-only, managed item has the token",
			items: []macKeychainItem{
				{service: "Claude Code-credentials", blob: mcpOnlyBlob},
				{service: "Orca Claude Code Managed Credentials", blob: accountBlob},
			},
			wantService: "Orca Claude Code Managed Credentials",
		},
		{
			// Order must not become a preference: a machine where the
			// classic item still holds the login keeps using it.
			name: "both items exist and the classic one has the token",
			items: []macKeychainItem{
				{service: "Claude Code-credentials", blob: accountBlob},
				{service: "Orca Claude Code Managed Credentials", blob: mcpOnlyBlob},
			},
			wantService: "Claude Code-credentials",
		},
		{
			name:    "no items at all",
			items:   nil,
			wantErr: ErrNotFound,
		},
		{
			// Nothing to read, but the write path still needs somewhere to
			// restore into, so the first existing item comes back with it.
			name:        "items exist but none holds a token",
			items:       []macKeychainItem{{service: "Claude Code-credentials", blob: mcpOnlyBlob}},
			wantService: "Claude Code-credentials",
			wantErr:     ErrNoAccountToken,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectLiveItem(c.items)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("selectLiveItem error = %v, want %v", err, c.wantErr)
			}
			if got.service != c.wantService {
				t.Errorf("selectLiveItem service = %q, want %q", got.service, c.wantService)
			}
		})
	}
}

func TestSelectLiveItemPrefersUnexpiredTokenInEitherOrder(t *testing.T) {
	t.Setenv("CUX_CREDS_BACKEND", "file")
	expired := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	valid := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"live","expiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli())

	for _, items := range [][]macKeychainItem{
		{{service: "Claude Code-credentials", blob: expired}, {service: "Claude Code-credentials", blob: valid}},
		{{service: "Claude Code-credentials", blob: valid}, {service: "Claude Code-credentials", blob: expired}},
	} {
		got, err := selectLiveItem(items)
		if err != nil {
			t.Fatalf("selectLiveItem: %v", err)
		}
		if got.blob != valid {
			t.Errorf("selectLiveItem blob = %q, want %q", got.blob, valid)
		}
	}
}

func TestSelectLiveItemKeepsServiceOrderBeforeExpiry(t *testing.T) {
	t.Setenv("CUX_CREDS_BACKEND", "file")
	expired := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	valid := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"live","expiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli())
	items := []macKeychainItem{
		{service: "Claude Code-credentials", blob: expired},
		{service: "Orca Claude Code Managed Credentials", blob: valid},
	}

	got, err := selectLiveItem(items)
	if err != nil {
		t.Fatalf("selectLiveItem: %v", err)
	}
	if got.service != "Claude Code-credentials" {
		t.Errorf("selectLiveItem service = %q, want %q", got.service, "Claude Code-credentials")
	}
}

func TestExtractAccessTokenDistinguishesMissingFromTokenless(t *testing.T) {
	if _, err := ExtractAccessToken(""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty blob: got %v, want ErrNotFound", err)
	}
	// The whole point of the split: "found, but not a login" must not
	// report itself as "not found" (issue #42).
	_, err := ExtractAccessToken(mcpOnlyBlob)
	if !errors.Is(err, ErrNoAccountToken) {
		t.Errorf("MCP-only blob: got %v, want ErrNoAccountToken", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("MCP-only blob must not also satisfy errors.Is(err, ErrNotFound)")
	}
	tok, err := ExtractAccessToken(accountBlob)
	if err != nil || tok != "tok" {
		t.Errorf("account blob: got (%q, %v), want (\"tok\", nil)", tok, err)
	}
}

func TestWriteRefusesBlobsThatWouldSignTheUserOut(t *testing.T) {
	// The guard rejects before any backend is reached, but isolate anyway so
	// a later refactor cannot turn this test into a write to the developer's
	// real keychain (issue #7).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

	for _, blob := range []string{mcpOnlyBlob, `{}`, `{"claudeAiOauth":{}}`} {
		if err := WriteLive(blob); !errors.Is(err, ErrNoAccountToken) {
			t.Errorf("WriteLive(%s) = %v, want ErrNoAccountToken", blob, err)
		}
		if err := WriteBackup(1, "a@x.test", blob); !errors.Is(err, ErrNoAccountToken) {
			t.Errorf("WriteBackup(%s) = %v, want ErrNoAccountToken", blob, err)
		}
	}
	// Empty stays its own, older error — it never reached storage anyway.
	if err := WriteLive(""); err == nil || errors.Is(err, ErrNoAccountToken) {
		t.Errorf("WriteLive(\"\") = %v, want the empty-credentials error", err)
	}
}

// ReadLive must answer the same way on every backend. The macOS path gets
// there by selecting on content; the file path needs the check in ReadLive
// itself, or Linux and Windows would hand a tokenless blob to callers and
// fail later, during an add, instead of here.
func TestReadLiveFileBackendReportsTokenlessCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

	if _, err := ReadLive(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no credentials file: got %v, want ErrNotFound", err)
	}

	// Written directly: WriteLive itself refuses this blob now.
	if err := os.MkdirAll(paths.ClaudeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentials(), []byte(mcpOnlyBlob), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLive(); !errors.Is(err, ErrNoAccountToken) {
		t.Fatalf("MCP-only credentials file: got %v, want ErrNoAccountToken", err)
	}

	if err := os.WriteFile(paths.ClaudeCredentials(), []byte(accountBlob), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLive()
	if err != nil || got != accountBlob {
		t.Fatalf("valid credentials file: got (%q, %v), want the blob and nil", got, err)
	}
}

func TestParseKeychainAccount(t *testing.T) {
	// Real `security find-generic-password` output, trimmed. The svce line
	// is included because a filtered dump interleaves acct and svce lines
	// from different items — this parser only ever sees one item's dump.
	const dump = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    0x00000007 <blob>=<NULL>
    "acct"<blob>="someone"
    "mdat"<timedate>=0x32303236303830345A00  "20260804Z\000"
    "svce"<blob>="Claude Code-credentials"
`
	if got := parseKeychainAccount(dump); got != "someone" {
		t.Errorf("parseKeychainAccount = %q, want %q", got, "someone")
	}
	// A NULL or hex-encoded acct must yield "" so the caller falls back to
	// $USER rather than writing under a mangled account name.
	for _, in := range []string{
		`    "acct"<blob>=<NULL>` + "\n",
		`    "acct"<blob>=0x736F6D65  "some"` + "\n",
		"attributes:\n",
	} {
		if got := parseKeychainAccount(in); got != "" {
			t.Errorf("parseKeychainAccount(%q) = %q, want \"\"", strings.TrimSpace(in), got)
		}
	}
}

func TestMultipleKeychainItemsPerServiceSelectsAccountTokenInEitherOrder(t *testing.T) {
	t.Setenv("CUX_CREDS_BACKEND", "file")
	const (
		service = "Claude Code-credentials"
		unknown = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="unknown"
    "svce"<blob>="Claude Code-credentials"
`
		marvin = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="marvin"
    "svce"<blob>="Claude Code-credentials"
`
	)

	for _, c := range []struct {
		dump string
		want string
	}{
		{unknown + marvin, "unknown,marvin"},
		{marvin + unknown, "marvin,unknown"},
	} {
		accounts := parseMacKeychainAccounts(c.dump, service)
		if got := strings.Join(accounts, ","); got != c.want {
			t.Fatalf("parseMacKeychainAccounts = %q, want %q", got, c.want)
		}
		items := make([]macKeychainItem, 0, len(accounts))
		for _, account := range accounts {
			blob := mcpOnlyBlob
			if account == "marvin" {
				blob = accountBlob
			}
			items = append(items, macKeychainItem{service: service, account: account, blob: blob})
		}

		got, err := selectLiveItem(items)
		if err != nil {
			t.Fatalf("selectLiveItem: %v", err)
		}
		if got.account != "marvin" {
			t.Errorf("selectLiveItem account = %q, want %q", got.account, "marvin")
		}
	}
}

func TestDecodeBackupValue(t *testing.T) {
	const blob = `{"claudeAiOauth":{}}`
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", blob, blob},
		{"base64", "go-keyring-base64:eyJjbGF1ZGVBaU9hdXRoIjp7fX0=", blob},
		{"hex", "go-keyring-encoded:7b22636c6175646541694f61757468223a7b7d7d", blob},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeBackupValue(c.in)
			if err != nil {
				t.Fatalf("decodeBackupValue(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("decodeBackupValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDecodeBackupValueRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"go-keyring-base64:!!!not-base64!!!",
		"go-keyring-encoded:zz",
	} {
		if _, err := decodeBackupValue(in); err == nil {
			t.Errorf("decodeBackupValue(%q): expected error, got nil", in)
		}
	}
}

// TestCheckBackupClassifiesWhatIsActuallyStored covers the question
// `cux status` could not answer: not "did a poll fail" but "is there a
// usable login here at all" (issue #46).
func TestCheckBackupClassifiesWhatIsActuallyStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

	// Nothing stored at all — the reporter's state once the keyring emptied.
	if got, _ := CheckBackup(1, "a@x.test"); got != BackupMissing {
		t.Fatalf("CheckBackup on an empty store = %v, want BackupMissing", got)
	}

	if err := WriteBackup(1, "a@x.test", accountBlob); err != nil {
		t.Fatal(err)
	}
	if got, err := CheckBackup(1, "a@x.test"); got != BackupOK || err != nil {
		t.Fatalf("CheckBackup on a good slot = %v (%v), want BackupOK", got, err)
	}

	// An MCP-only blob is stored but would sign the user out (issue #42).
	// WriteBackup refuses those, so write it underneath to model a slot
	// captured by an older build.
	if err := writeBackupFile(2, "b@x.test", mcpOnlyBlob); err != nil {
		t.Fatal(err)
	}
	if got, _ := CheckBackup(2, "b@x.test"); got != BackupNoToken {
		t.Fatalf("CheckBackup on a token-less slot = %v, want BackupNoToken", got)
	}

	for _, bs := range []BackupState{BackupOK, BackupMissing, BackupNoToken, BackupUnreadable} {
		if bs.Describe() == "unknown" {
			t.Errorf("BackupState %d has no description", bs)
		}
	}
}

// TestSlotHoldingTokenFindsAMisfiledToken covers the check that guards every
// write path: a token already stored under another slot cannot belong to the
// account being written (issue #46).
func TestSlotHoldingTokenFindsAMisfiledToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

	blobA := `{"claudeAiOauth":{"accessToken":"tok-a","refreshToken":"r"}}`
	blobB := `{"claudeAiOauth":{"accessToken":"tok-b","refreshToken":"r"}}`
	if err := WriteBackup(1, "a@x.test", blobA); err != nil {
		t.Fatal(err)
	}
	if err := WriteBackup(2, "b@x.test", blobB); err != nil {
		t.Fatal(err)
	}
	slots := []SlotRef{{1, "a@x.test"}, {2, "b@x.test"}}

	// Writing a's token into slot 2 is the corruption — slot 1 already has it.
	if ref, ok := SlotHoldingToken(slots, 2, blobA); !ok || ref.Slot != 1 {
		t.Fatalf("SlotHoldingToken = %+v (%v), want slot 1", ref, ok)
	}
	// Re-writing a slot's own token is the ordinary refresh, not a collision.
	if ref, ok := SlotHoldingToken(slots, 1, blobA); ok {
		t.Fatalf("a slot's own token reported as a collision: %+v", ref)
	}
	// A genuinely new token for slot 2 is fine.
	fresh := `{"claudeAiOauth":{"accessToken":"tok-b2","refreshToken":"r"}}`
	if ref, ok := SlotHoldingToken(slots, 2, fresh); ok {
		t.Fatalf("a rotated token reported as a collision: %+v", ref)
	}
}

// TestSharedTokenSlotsFindsAnAlreadyCorruptedPool is the diagnostic side:
// pools captured by a build with no write guard are already in this state and
// nothing else would say so.
func TestSharedTokenSlotsFindsAnAlreadyCorruptedPool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")

	same := `{"claudeAiOauth":{"accessToken":"one-token","refreshToken":"r"}}`
	other := `{"claudeAiOauth":{"accessToken":"distinct","refreshToken":"r"}}`
	for _, w := range []struct {
		slot  int
		email string
		blob  string
	}{{1, "a@x.test", same}, {2, "b@x.test", same}, {3, "c@x.test", other}} {
		if err := WriteBackup(w.slot, w.email, w.blob); err != nil {
			t.Fatal(err)
		}
	}
	slots := []SlotRef{{1, "a@x.test"}, {2, "b@x.test"}, {3, "c@x.test"}}
	groups := SharedTokenSlots(slots)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if len(groups[0]) != 2 || groups[0][0].Slot != 1 || groups[0][1].Slot != 2 {
		t.Fatalf("group = %+v, want slots 1 and 2", groups[0])
	}
	// A healthy pool reports nothing.
	if got := SharedTokenSlots([]SlotRef{{3, "c@x.test"}}); len(got) != 0 {
		t.Fatalf("healthy pool reported %+v", got)
	}
}
