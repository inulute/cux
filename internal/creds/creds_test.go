package creds

import (
	"errors"
	"os"
	"strings"
	"testing"

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
