package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/inulute/cux/internal/paths"
)

func writeOAuthBackup(t *testing.T, slot int, email, acctUUID, orgUUID string) {
	t.Helper()
	dir := paths.AccountDir(slot, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"emailAddress": email, "accountUuid": acctUUID, "organizationUuid": orgUUID,
	})
	if err := os.WriteFile(filepath.Join(dir, "oauth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Seats added before cux recorded identities key on their email address, so
// two seats on one address share a usage-cache entry and overwrite each
// other's readings (#24). The identity was in each seat's own oauth backup
// the whole time.
func TestBackfillIdentitiesFromTheOAuthBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	writeOAuthBackup(t, 1, "me@x.test", "u-personal", "")
	writeOAuthBackup(t, 2, "me@x.test", "u-me", "o-corp")

	st := &State{
		ActiveSlot: 1,
		Accounts: map[int]Account{
			1: {Slot: 1, Email: "me@x.test"},
			2: {Slot: 2, Email: "me@x.test"},
		},
	}
	if a, b := st.Accounts[1].CacheKey(), st.Accounts[2].CacheKey(); a != b {
		t.Fatalf("precondition: twins should collide before the backfill, got %q and %q", a, b)
	}

	renames := st.BackfillIdentities()

	if got := st.Accounts[2].CacheKey(); got != "u-me|o-corp" {
		t.Errorf("org seat key = %q, want the uuid pair", got)
	}
	// A personal plan reports no organizationUuid, so its key stays the
	// address — but the org twin has moved off it, so they no longer collide.
	if a, b := st.Accounts[1].CacheKey(), st.Accounts[2].CacheKey(); a == b {
		t.Errorf("twins still share key %q after the backfill", a)
	}
	if got, ok := renames["me@x.test"]; !ok || got != "u-me|o-corp" {
		t.Errorf("renames[me@x.test] = %q (%v), want the org seat's new key", got, ok)
	}
}

// A reading two seats shared describes neither, so it is dropped rather than
// handed to whichever of them happened to move.
func TestBackfillDropsAReadingTwoSeatsShared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	writeOAuthBackup(t, 1, "me@x.test", "u-a", "o-a")
	writeOAuthBackup(t, 2, "me@x.test", "u-b", "o-b")

	st := &State{Accounts: map[int]Account{
		1: {Slot: 1, Email: "me@x.test"},
		2: {Slot: 2, Email: "me@x.test"},
	}}
	if got, ok := st.BackfillIdentities()["me@x.test"]; !ok || got != "" {
		t.Errorf("renames[me@x.test] = %q (%v), want \"\" so the caller drops it", got, ok)
	}
}

// Nothing to repair must cost nothing and change nothing.
func TestBackfillIsANoOpWhenIdentitiesAreKnown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	st := &State{Accounts: map[int]Account{
		1: {Slot: 1, Email: "a@x.test", UUID: "u-a", OrgUUID: "o-a"},
	}}
	if renames := st.BackfillIdentities(); len(renames) != 0 {
		t.Errorf("renames = %v, want none", renames)
	}
	if st.Accounts[1].CacheKey() != "u-a|o-a" {
		t.Errorf("key changed: %q", st.Accounts[1].CacheKey())
	}
}
