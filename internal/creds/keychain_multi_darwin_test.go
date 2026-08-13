//go:build darwin

package creds

import (
	"os"
	"os/exec"
	"testing"
)

// Reproduces the layout in PR #45 against a real keychain: one service
// holding several generic-password items, where the item Claude Code
// actually writes to is not the one `security find-generic-password -s`
// happens to return.
//
// Deliberately self-contained — its own service name, accounts, blobs and
// helpers — so the same file can be dropped onto either 0.3.8 or the #45
// branch without depending on test fixtures either one might rename.
//
// Opt-in like the other real-keychain tests: CUX_TEST_REAL_KEYCHAIN=1.
const (
	multiService   = "cux-test-multi-credentials"
	multiAcctStale = "cux-test-acct-stale"
	multiAcctLive  = "cux-test-acct-live"

	// The leftover item: real MCP state, no account login.
	multiStaleBlob = `{"mcpOAuth":{"some-server":{"accessToken":"mcp-only"}}}`
	// The item Claude Code is actively writing to.
	multiLiveBlob = `{"claudeAiOauth":{"accessToken":"live-tok","refreshToken":"r"}}`
)

func requireRealKeychainMulti(t *testing.T) {
	t.Helper()
	if os.Getenv("CUX_TEST_REAL_KEYCHAIN") == "" {
		t.Skip("set CUX_TEST_REAL_KEYCHAIN=1 to run tests that write to the real keychain")
	}
	t.Setenv("CUX_CREDS_BACKEND", "")
}

func addMultiItem(t *testing.T, service, account, blob string) {
	t.Helper()
	out, err := exec.Command("security", "add-generic-password",
		"-U", "-s", service, "-a", account, "-w", blob).CombinedOutput()
	if err != nil {
		t.Fatalf("seeding %s/%s: %v (%s)", service, account, err, out)
	}
}

func cleanupMultiService(t *testing.T, service string) {
	t.Helper()
	t.Cleanup(func() {
		// Drain every item under the service, however many accumulated.
		for {
			if err := exec.Command("security", "delete-generic-password",
				"-s", service).Run(); err != nil {
				return
			}
		}
	})
}

func TestRealKeychainFindsTheLoginWhenOneServiceHoldsSeveralItems(t *testing.T) {
	requireRealKeychainMulti(t)
	cleanupMultiService(t, multiService)

	// Order matters: seed the tokenless leftover first, which is the
	// position the reporter's older item was in.
	addMultiItem(t, multiService, multiAcctStale, multiStaleBlob)
	addMultiItem(t, multiService, multiAcctLive, multiLiveBlob)

	restore := macKeychainServices
	macKeychainServices = []string{multiService}
	t.Cleanup(func() { macKeychainServices = restore })

	// What the name-only lookup actually hands back. Compiles against both
	// the 0.3.8 signature and #45's variadic one.
	if blob, err := readMacKeychainSecret(multiService); err == nil {
		t.Logf("name-only lookup: hasToken=%v (this is what 0.3.8 selects from)",
			hasAccountToken(blob))
	} else {
		t.Logf("name-only lookup errored: %v", err)
	}

	// The claim under test: enumeration must see the whole candidate set.
	items, err := findMacKeychainItems()
	if err != nil {
		t.Fatalf("findMacKeychainItems: %v", err)
	}
	t.Logf("findMacKeychainItems returned %d item(s) for one service", len(items))
	if len(items) < 2 {
		t.Errorf("found %d item(s) under a service holding 2; the login is invisible to selection", len(items))
	}

	// The consequence that matters: the account token must be reachable.
	got, err := ReadLive()
	if err != nil {
		t.Fatalf("ReadLive: %v — the live login was not found despite existing in the keychain", err)
	}
	if got != multiLiveBlob {
		t.Errorf("ReadLive returned the wrong item:\n got %q\nwant %q", got, multiLiveBlob)
	}
}
