package monitor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
)

func TestSyncLiveIfActiveWritesOnlyMatchingLiveAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live credential file test is Linux-only")
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))
	t.Setenv("CUX_CREDS_BACKEND", "file")

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
	t.Setenv("CUX_CREDS_BACKEND", "file")

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
	t.Setenv("CUX_CREDS_BACKEND", "file")

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
