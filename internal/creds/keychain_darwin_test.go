//go:build darwin

package creds

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Round-trip tests against the real macOS keychain, reproducing the issue #42
// layout: two credentials items, where the one cux used to look up by name
// holds only the MCP block and the account login lives in the other.
//
// These write to the login keychain, so they are opt-in: set
// CUX_TEST_REAL_KEYCHAIN=1 to run them. Items are created under
// cux-test-* service names — never Claude Code's own — and removed on
// cleanup. macOS may prompt for keychain access the first time.
const envRealKeychainTests = "CUX_TEST_REAL_KEYCHAIN"

const (
	testServiceClassic = "cux-test-classic-credentials"
	testServiceManaged = "cux-test-managed-credentials"
	// Deliberately not $USER: the account name is what `add-generic-password
	// -U` matches on, so a write that assumes $USER lands in a new item
	// instead of this one.
	testAcctClassic = "cux-test-acct-classic"
	testAcctManaged = "cux-test-acct-managed"
)

func requireRealKeychain(t *testing.T) {
	t.Helper()
	if os.Getenv(envRealKeychainTests) == "" {
		t.Skipf("set %s=1 to run tests that write to the real keychain", envRealKeychainTests)
	}
	// The keychain path is only taken when the file backend is not forced.
	t.Setenv(envCredsBackend, "")
}

func addTestKeychainItem(t *testing.T, service, account, blob string) {
	t.Helper()
	out, err := exec.Command("security", "add-generic-password",
		"-U", "-s", service, "-a", account, "-w", blob).CombinedOutput()
	if err != nil {
		t.Fatalf("seeding %s: %v (%s)", service, err, out)
	}
	t.Cleanup(func() {
		// Remove every item under this service, so a duplicate created by a
		// regression is cleaned up too rather than left behind.
		for {
			if err := exec.Command("security", "delete-generic-password",
				"-s", service).Run(); err != nil {
				return
			}
		}
	})
}

func readTestKeychainItem(t *testing.T, service string) string {
	t.Helper()
	blob, err := readMacKeychainSecret(service)
	if err != nil {
		t.Fatalf("reading back %s: %v", service, err)
	}
	return blob
}

// itemExistsForAccount reports whether an item exists under this exact
// (service, account) pair — the pair `-U` matches on. Used to prove a write
// updated the existing item instead of creating a second one beside it.
func itemExistsForAccount(service, account string) bool {
	err := exec.Command("security", "find-generic-password",
		"-s", service, "-a", account).Run()
	return err == nil
}

func TestRealKeychainSelectsAndWritesTheItemHoldingTheLogin(t *testing.T) {
	requireRealKeychain(t)

	// The #42 layout: the item cux looked up by name has no account token.
	addTestKeychainItem(t, testServiceClassic, testAcctClassic, mcpOnlyBlob)
	addTestKeychainItem(t, testServiceManaged, testAcctManaged, accountBlob)

	restore := macKeychainServices
	macKeychainServices = []string{testServiceClassic, testServiceManaged}
	t.Cleanup(func() { macKeychainServices = restore })

	// Confirm the trap the old code fell into is really set up: a name-only
	// lookup of the first service succeeds and yields no account token.
	if blob := readTestKeychainItem(t, testServiceClassic); hasAccountToken(blob) {
		t.Fatal("fixture is wrong: the classic item should have no account token")
	}

	// Read: selection must cross to the second item.
	items, err := findMacKeychainItems()
	if err != nil {
		t.Fatalf("findMacKeychainItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("found %d items, want 2", len(items))
	}
	selected, err := selectLiveItem(items)
	if err != nil {
		t.Fatalf("selectLiveItem: %v", err)
	}
	if selected.service != testServiceManaged {
		t.Errorf("selected %q, want %q", selected.service, testServiceManaged)
	}
	got, err := ReadLive()
	if err != nil || got != accountBlob {
		t.Fatalf("ReadLive = (%q, %v), want the account blob and nil", got, err)
	}

	// The account name must come back from the item itself, not $USER.
	if acct := macKeychainAccount(testServiceManaged); acct != testAcctManaged {
		t.Errorf("macKeychainAccount = %q, want %q", acct, testAcctManaged)
	}

	// Write: must land in the item holding the login, under its own account.
	const updated = `{"claudeAiOauth":{"accessToken":"tok2","refreshToken":"r2"}}`
	if err := WriteLive(updated); err != nil {
		t.Fatalf("WriteLive: %v", err)
	}
	if blob := readTestKeychainItem(t, testServiceManaged); blob != updated {
		t.Errorf("managed item = %q, want the updated blob", blob)
	}
	// The MCP-only item must be untouched — cux used to overwrite it, which
	// destroyed the MCP OAuth state on an affected machine.
	if blob := readTestKeychainItem(t, testServiceClassic); blob != mcpOnlyBlob {
		t.Errorf("classic item = %q, want it untouched", blob)
	}
	// No duplicate: nothing may exist under (managed service, $USER).
	if user := os.Getenv("USER"); user != "" && user != testAcctManaged {
		if itemExistsForAccount(testServiceManaged, user) {
			t.Errorf("write created a duplicate item under acct %q instead of updating acct %q", user, testAcctManaged)
		}
	}
	if !itemExistsForAccount(testServiceManaged, testAcctManaged) {
		t.Errorf("item under acct %q disappeared", testAcctManaged)
	}

	// And the guard holds against the real backend: an MCP-only blob must
	// not reach the keychain, or it would sign the user out.
	if err := WriteLive(mcpOnlyBlob); !errors.Is(err, ErrNoAccountToken) {
		t.Errorf("WriteLive(mcp-only) = %v, want ErrNoAccountToken", err)
	}
	if blob := readTestKeychainItem(t, testServiceManaged); blob != updated {
		t.Errorf("refused write still changed the item: %q", blob)
	}
}

// A first-ever write, with no item present, must create one under the
// primary service so a fresh install still works.
func TestRealKeychainFirstWriteCreatesPrimaryItem(t *testing.T) {
	requireRealKeychain(t)

	restore := macKeychainServices
	macKeychainServices = []string{testServiceClassic, testServiceManaged}
	t.Cleanup(func() { macKeychainServices = restore })
	t.Cleanup(func() {
		for {
			if err := exec.Command("security", "delete-generic-password",
				"-s", testServiceClassic).Run(); err != nil {
				return
			}
		}
	})

	if _, err := ReadLive(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("precondition: ReadLive = %v, want ErrNotFound (leftover test item?)", err)
	}
	if err := WriteLive(accountBlob); err != nil {
		t.Fatalf("WriteLive: %v", err)
	}
	if blob := readTestKeychainItem(t, testServiceClassic); blob != accountBlob {
		t.Errorf("primary item = %q, want the account blob", blob)
	}
	// With nothing to read an account name from, $USER is the fallback.
	if user := os.Getenv("USER"); user != "" {
		if acct := macKeychainAccount(testServiceClassic); acct != user {
			t.Errorf("macKeychainAccount = %q, want $USER %q", acct, user)
		}
	}
	got, err := ReadLive()
	if err != nil || !strings.Contains(got, "tok") {
		t.Fatalf("ReadLive after first write = (%q, %v)", got, err)
	}
}
