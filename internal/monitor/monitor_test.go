package monitor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
)

// writeClaudeConfig writes a minimal Claude Code config whose
// oauthAccount block names the given active account.
func writeClaudeConfig(t *testing.T, dir, email, org string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"oauthAccount":{"emailAddress":"` + email + `","accountUuid":"uuid-x","organizationUuid":"` + org + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// usageServer returns a fake usage API that answers 200 only for the
// one access token it is told to accept; every other token gets 500,
// standing in for the way Anthropic rejects a stale access token.
func usageServer(t *testing.T, acceptToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+acceptToken {
			http.Error(w, "stale token", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42},"seven_day":{"utilization":50}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncLiveIfActiveWritesOnlyMatchingLiveAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live credential file test is Linux-only")
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"oauthAccount":{"emailAddress":"active@example.com","accountUuid":"uuid-active"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := creds.WriteLive(`{"claudeAiOauth":{"accessToken":"old"}}`); err != nil {
		t.Fatal(err)
	}

	ok, err := syncLiveIfActive("other@example.com", "", `{"claudeAiOauth":{"accessToken":"wrong"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("syncLiveIfActive reported sync for non-active account")
	}
	got, err := creds.ReadLive()
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"claudeAiOauth":{"accessToken":"old"}}` {
		t.Fatalf("live credentials changed for non-active account: %s", got)
	}

	ok, err = syncLiveIfActive("active@example.com", "", `{"claudeAiOauth":{"accessToken":"fresh"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("syncLiveIfActive did not report sync for active account")
	}
	got, err = creds.ReadLive()
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"claudeAiOauth":{"accessToken":"fresh"}}` {
		t.Fatalf("live credentials were not refreshed: %s", got)
	}
}

func TestRefreshAllCreatesDataDirBeforeLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	if _, err := os.Stat(paths.BackupRoot()); !os.IsNotExist(err) {
		t.Fatalf("backup root should not exist before refresh, stat err = %v", err)
	}

	cache, errs := RefreshAll()
	if len(errs) != 0 {
		t.Fatalf("RefreshAll returned errors: %v", errs)
	}
	if cache == nil {
		t.Fatal("RefreshAll returned nil cache")
	}
	if _, err := os.Stat(paths.LockFile()); err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
}

func TestRefreshAllReportsUnreadableUsageCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	runtimeDir := paths.RuntimeDir()
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(runtimeDir, "usage-cache.json")
	if err := os.WriteFile(cachePath, []byte(`{bad json`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errs := RefreshAll(); len(errs) == 0 {
		t.Fatal("RefreshAll should report corrupt usage cache")
	} else if !strings.Contains(errs[0].Error(), "usage: parse cache") {
		t.Fatalf("RefreshAll error = %v, want usage cache parse error", errs[0])
	}
}

const (
	staleBlob = `{"claudeAiOauth":{"accessToken":"stale-token","refreshToken":"r"}}`
	freshBlob = `{"claudeAiOauth":{"accessToken":"fresh-token","refreshToken":"r"}}`
)

// TestRefreshOnePrefersLiveBlobForActiveAccount is the regression test
// for the active account never refreshing: its per-account backup is
// only rewritten on switch, so its access token goes stale and the
// usage API rejects it. refreshOne must read the freshly-rotated token
// from the live credentials file for whichever account is active.
func TestRefreshOnePrefersLiveBlobForActiveAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live credential file test is Linux-only")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	srv := usageServer(t, "fresh-token")
	t.Setenv("CUX_USAGE_ENDPOINT", srv.URL)

	// This account is the active one, and only the live file has the
	// token the API still accepts; the backup is stale.
	writeClaudeConfig(t, dir, "active@example.com", "org-1")
	if err := creds.WriteBackup(1, "active@example.com", staleBlob); err != nil {
		t.Fatal(err)
	}
	if err := creds.WriteLive(freshBlob); err != nil {
		t.Fatal(err)
	}

	u, err := refreshOne(1, "active@example.com", "org-1")
	if err != nil {
		t.Fatalf("refreshOne errored for active account: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 42 {
		t.Fatalf("refreshOne returned %+v, want five_hour utilization 42", u)
	}
	// The stale backup should have been refreshed from the live token.
	got, err := creds.ReadBackup(1, "active@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != freshBlob {
		t.Fatalf("backup not refreshed from live token: %s", got)
	}
}

// TestRefreshOneUsesBackupForInactiveAccount confirms the live-blob
// preference is scoped to the active account only: an inactive account
// must still be refreshed from its own per-account backup, never from
// the unrelated live credentials.
func TestRefreshOneUsesBackupForInactiveAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live credential file test is Linux-only")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	srv := usageServer(t, "fresh-token")
	t.Setenv("CUX_USAGE_ENDPOINT", srv.URL)

	// A different account is active. The live file holds a token the
	// API rejects; only the inactive account's own backup is valid.
	writeClaudeConfig(t, dir, "active@example.com", "org-1")
	if err := creds.WriteLive(staleBlob); err != nil {
		t.Fatal(err)
	}
	if err := creds.WriteBackup(2, "inactive@example.com", freshBlob); err != nil {
		t.Fatal(err)
	}

	u, err := refreshOne(2, "inactive@example.com", "org-2")
	if err != nil {
		t.Fatalf("refreshOne errored for inactive account: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 42 {
		t.Fatalf("refreshOne returned %+v, want five_hour utilization 42", u)
	}
}

// TestLiveBlobForMatchesActiveAccountOnly checks the identity guard:
// the live blob is returned only for the exact active email+org, so a
// different account's token can never be used.
func TestLiveBlobForMatchesActiveAccountOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live credential file test is Linux-only")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))

	writeClaudeConfig(t, dir, "active@example.com", "org-1")
	if err := creds.WriteLive(freshBlob); err != nil {
		t.Fatal(err)
	}

	if blob, ok := liveBlobFor("active@example.com", "org-1"); !ok || blob != freshBlob {
		t.Fatalf("liveBlobFor(active, org-1) = %q,%v; want the live blob,true", blob, ok)
	}
	if _, ok := liveBlobFor("other@example.com", ""); ok {
		t.Fatal("liveBlobFor returned a blob for a non-active email")
	}
	if _, ok := liveBlobFor("active@example.com", "wrong-org"); ok {
		t.Fatal("liveBlobFor returned a blob when the org did not match")
	}
}
